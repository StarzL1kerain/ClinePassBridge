package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Cline 的额度信息来自三个只读接口（2026-09 用真实账号实测）：
//
//	GET {base}/users/{userId}/balance    -> {balance}                余额
//	GET {base}/users/me/plan             -> 套餐、周期与窗口上限
//	GET {base}/users/{userId}/usages     -> 逐条用量（本文件未使用）
//
// 两个单位不同，均已实证：
//   - balance 单位为 1e-6 美元。网页端把 -32221 显示为 Credits: -0.0322。
//   - costUsd 与 inferenceCapThreshold 单位为 1e-8 美元。按该单位换算，三个窗口上限
//     分别是 10 / 25 / 50 美元，与月费 9.99 美元的订阅相符；若按 1e-6 则会得到
//     1000 / 2500 / 5000 美元，显然不成立。
const (
	balanceUnitUSD   = 1e-6
	usageCostUnitUSD = 1e-8
)

// capWindowKeys 是套餐权益里的三个滚动窗口上限，顺序即展示顺序。
var capWindowKeys = []struct {
	key   string
	label string
}{
	{"last5HoursUsageCostUSDPerUser", "5 小时额度上限"},
	{"last7daysUsageCostUSDPerUser", "7 天额度上限"},
	{"last30daysUsageCostUSDPerUser", "30 天额度上限"},
}

// quotaWindows 把上游 usage-limits 的窗口类型映射为宿主管理面板能识别的 window token。
// 面板会把 token 翻译成中文标签（five_hour → 5 小时限额、seven_day → 7 天限额）；
// 认不出的 token 会退化成内部 id，所以这里的取值不能随意改。
var quotaWindows = []struct {
	apiType string
	window  string
	label   string
}{
	{"five_hour", "five_hour", "5 小时"},
	{"weekly", "seven_day", "7 天"},
	{"monthly", "monthly", "30 天"},
}

// usageLimit 是 GET /users/me/plan/usage-limits 返回的单条窗口用量。
type usageLimit struct {
	Type        string  `json:"type"`
	PercentUsed float64 `json:"percentUsed"`
	ResetsAt    string  `json:"resetsAt"`
}

type usageLimits struct {
	Limits []usageLimit `json:"limits"`
}

// quotaRequest 对应宿主的 QuotaFetchRequest / QuotaResetRequest。
type quotaRequest struct {
	AuthIndex      string  `json:"auth_index"`
	AuthID         string  `json:"auth_id"`
	Provider       string  `json:"provider"`
	StorageJSON    []byte  `json:"storage_json"`
	Metadata       any     `json:"metadata"`
	HostCallbackID string  `json:"host_callback_id"`
	AuthIndexCamel *string `json:"authIndex"`
	AuthIDPascal   *string `json:"AuthID"`
}

func (r quotaRequest) credentialID() string {
	for _, candidate := range []string{r.AuthID, r.AuthIndex, valueOrEmpty(r.AuthIDPascal), valueOrEmpty(r.AuthIndexCamel)} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// quotaDescribe 声明本插件支持的 provider 与是否支持重置。
// 宿主侧 QuotaDescribeResponse 的字段标签是 snake_case 且没有自定义反序列化，
// 因此这里同时给出 camelCase 别名作为兜底：多余字段会被 encoding/json 忽略。
func quotaDescribe() any {
	return map[string]any{
		"supported_providers": []string{Provider},
		"SupportedProviders":  []string{Provider},
		"display_name":        "Cline Pass 额度",
		"displayName":         "Cline Pass 额度",
		"supports_reset":      false,
		"supportsReset":       false,
	}
}

func quotaUnsupportedReset() any {
	return map[string]any{"success": false, "message": "Cline 未提供重置额度的接口"}
}

func (s *Service) quotaCredential(storage []byte, credentialID string) (Credential, error) {
	var c Credential
	if len(storage) > 0 {
		_ = json.Unmarshal(storage, &c)
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[credentialID]; ok {
		c = current
	}
	s.mu.RUnlock()
	if c.bearerToken() == "" || c.Disabled {
		return c, fail(401, "Cline Pass 凭据缺失或已停用")
	}
	return c, nil
}

// clineRequest 发出一次 Cline API GET，返回状态码与响应体。
func (s *Service) clineRequest(callbackID, path, token string) (int, []byte, error) {
	headers := http.Header{
		"Accept":        []string{"application/json"},
		"Authorization": []string{"Bearer " + token},
	}
	status, body, err := s.hostRequest(callbackID, http.MethodGet, s.config().BaseURL+path, headers, nil)
	if err != nil {
		return status, body, fail(502, "无法连接 Cline 服务："+safeError(err))
	}
	return status, body, nil
}

// decodeQuotaBody 解开 {success, data} 信封。
// 401 会被换成可操作的提示：上游原文只说 "re-authenticate your Cline account"，
// 从字面上看不出其实是插件续期的问题。
func decodeQuotaBody(status int, body []byte, out any) error {
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   json.RawMessage `json:"error"`
	}
	if e := json.Unmarshal(body, &env); e != nil {
		return fail(502, "Cline 返回了无效 JSON")
	}
	if status < 200 || status >= 300 || !env.Success {
		if statusOr(status, 502) == http.StatusUnauthorized {
			return fail(401, "Cline 令牌已失效且自动续期未成功，请在插件控制台重新登录 Cline 账号")
		}
		return fail(statusOr(status, 502), "读取额度失败："+envelopeMessage(env.Error))
	}
	if out != nil && len(env.Data) > 0 {
		if e := json.Unmarshal(env.Data, out); e != nil {
			return fail(502, "解析额度响应失败")
		}
	}
	return nil
}

// quotaGet 以凭据访问 Cline API。access_token 只有 1 小时有效期，过期后上游返回 401；
// 这里遇到 401 就强制续期并用新令牌重试一次，避免额度刷新被永久卡死。
// 返回可能已续期的凭据，供随后的调用复用新令牌。
func (s *Service) quotaGet(c Credential, callbackID, path string, out any) (Credential, error) {
	status, body, err := s.clineRequest(callbackID, path, c.bearerToken())
	if err != nil {
		return c, err
	}
	if status == http.StatusUnauthorized && c.kind() == AuthKindOAuth && strings.TrimSpace(c.RefreshToken) != "" {
		renewed, renewErr := s.renewOAuthCredential(c, callbackID)
		if renewErr != nil {
			s.logCredentialEvent(c, http.StatusUnauthorized, "Cline 令牌已过期，自动续期失败："+safeError(renewErr))
			return c, fail(401, "Cline 令牌已过期且自动续期失败，请在插件控制台重新登录 Cline 账号")
		}
		c = renewed
		if status, body, err = s.clineRequest(callbackID, path, c.bearerToken()); err != nil {
			return c, err
		}
	}
	return c, decodeQuotaBody(status, body, out)
}

type clineUser struct {
	ID string `json:"id"`
}

type clinePlanInfo struct {
	Plan struct {
		Name         string `json:"name"`
		DisplayName  string `json:"displayName"`
		Interval     string `json:"interval"`
		IsActive     bool   `json:"isActive"`
		Entitlements map[string]struct {
			Enabled               bool               `json:"enabled"`
			InferenceCapThreshold map[string]float64 `json:"inferenceCapThreshold"`
		} `json:"entitlements"`
	} `json:"plan"`
	CurrentPeriodStart string `json:"currentPeriodStart"`
	CurrentPeriodEnd   string `json:"currentPeriodEnd"`
}

type clineBalance struct {
	Balance float64 `json:"balance"`
}

// fetchQuota 汇总套餐、余额与三个滚动窗口的已用比例。
//
// 关键的窗口数据来自 GET /users/me/plan/usage-limits：上游直接给出 percentUsed 与 resetsAt，
// 不需要翻页聚合逐条用量（逐条列表每页上限 200 条且无时间过滤，7 天窗口要上百次请求）。
func (s *Service) fetchQuota(raw json.RawMessage) (any, error) {
	var r quotaRequest
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	c, e := s.quotaCredential(r.StorageJSON, r.credentialID())
	if e != nil {
		return nil, e
	}
	if e := s.begin(); e != nil {
		return nil, e
	}
	defer s.active.Done()
	// 额度请求同样要先续期：access_token 只有 1 小时有效期，而续期此前只挂在模型请求
	// 路径上（executor.prepare），于是长时间不用之后每次刷新额度都拿着过期令牌打上游、固定 401。
	c = s.renewIfNeeded(c, r.HostCallbackID)

	// /users/me/plan 只提供套餐名、窗口上限与当前周期，属于展示增强项。
	// 用 API key 认证时该接口可能不开放（第三方用量工具同样只读 usage-limits），
	// 那种情况（404/405 等）降级处理：记一条日志，窗口照常展示。
	// 但 401/403 是凭据层面的问题（过期、续期失败、无权限），必须照旧报错 ——
	// 否则会把"令牌坏了"伪装成"只是少了套餐名"。
	var plan clinePlanInfo
	switch planned, planErr := s.quotaGet(c, r.HostCallbackID, "/users/me/plan", &plan); {
	case planErr == nil:
		c = planned
	case statusOf(planErr) == 401 || statusOf(planErr) == 403:
		return nil, planErr
	default:
		s.logCredentialEvent(c, statusOf(planErr), "套餐信息 /users/me/plan 不可用，仅展示额度窗口")
		plan = clinePlanInfo{}
	}
	var limits usageLimits
	if c, e = s.quotaGet(c, r.HostCallbackID, "/users/me/plan/usage-limits", &limits); e != nil {
		return nil, e
	}
	if len(limits.Limits) == 0 {
		return nil, fail(502, "Cline 未返回任何额度窗口")
	}

	// 余额只用于摘要展示；取不到时不影响额度窗口。
	var balance clineBalance
	account := strings.TrimSpace(c.AccountID)
	if account == "" {
		var user clineUser
		if renewed, e := s.quotaGet(c, r.HostCallbackID, "/users/me", &user); e == nil {
			c = renewed
			account = strings.TrimSpace(user.ID)
		}
	}
	hasBalance := false
	if account != "" {
		if _, e := s.quotaGet(c, r.HostCallbackID, "/users/"+account+"/balance", &balance); e == nil {
			hasBalance = true
		}
	}
	return buildQuotaResponse(plan, limits, balance, hasBalance, time.Now()), nil
}

func buildQuotaResponse(plan clinePlanInfo, limits usageLimits, balance clineBalance, hasBalance bool, now time.Time) any {
	byType := map[string]usageLimit{}
	for _, limit := range limits.Limits {
		byType[limit.Type] = limit
	}
	buckets := []any{}
	for _, window := range quotaWindows {
		limit, ok := byType[window.apiType]
		if !ok {
			continue
		}
		// 上游用 percentUsed 表示已用比例，宿主只认 remainingFraction。
		remaining := 1 - limit.PercentUsed/100
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 1 {
			remaining = 1
		}
		bucket := map[string]any{
			"window":            window.window,
			"remainingFraction": remaining,
			"description":       fmt.Sprintf("%s窗口已用 %.1f%%", window.label, limit.PercentUsed),
		}
		if reset := strings.TrimSpace(limit.ResetsAt); reset != "" {
			bucket["resetTime"] = reset
		}
		buckets = append(buckets, bucket)
	}

	summary := []any{}
	if hasBalance {
		summary = append(summary, quotaCurrencyMetric("cline_pass_balance", "余额", balance.Balance*balanceUnitUSD))
	}
	thresholds := map[string]float64{}
	if entitlement, ok := plan.Plan.Entitlements["cline_pass"]; ok {
		thresholds = entitlement.InferenceCapThreshold
	}
	for _, window := range capWindowKeys {
		if amount, ok := thresholds[window.key]; ok {
			summary = append(summary, quotaCurrencyMetric("cline_pass_cap_"+window.key, window.label, amount*usageCostUnitUSD))
		}
	}
	if end, err := time.Parse(time.RFC3339, strings.TrimSpace(plan.CurrentPeriodEnd)); err == nil {
		days := end.Sub(now).Hours() / 24
		if days < 0 {
			days = 0
		}
		summary = append(summary, map[string]any{
			"key":    "cline_pass_period_days_left",
			"label":  "当前周期剩余天数",
			"value":  float64(int(days*10)) / 10,
			"unit":   "天",
			"format": "number",
		})
	}

	response := map[string]any{
		"groups": []any{map[string]any{"displayName": "Cline Pass 额度", "buckets": buckets}},
	}
	if len(summary) > 0 {
		response["summary"] = summary
	}
	planName := strings.TrimSpace(plan.Plan.DisplayName)
	if planName == "" {
		planName = strings.TrimSpace(plan.Plan.Name)
	}
	if planName != "" || plan.Plan.Interval != "" {
		response["subscription"] = map[string]any{
			"plan":     planName,
			"tierName": strings.TrimSpace(plan.Plan.Interval),
		}
	}
	return response
}

// quotaCurrencyMetric 构造一条货币指标。宿主只接受 ISO 4217 大写代码。
func quotaCurrencyMetric(key, label string, value float64) map[string]any {
	return map[string]any{
		"key":      key,
		"label":    label,
		"value":    value,
		"format":   "currency",
		"currency": "USD",
	}
}

// managementQuota 供插件管理页直接查看，便于在不依赖宿主 UI 的情况下核对额度。
func (s *Service) managementQuota(credentialID string) any {
	s.mu.RLock()
	c, ok := s.creds[credentialID]
	if !ok && credentialID == "" {
		for _, candidate := range s.creds {
			c, ok = candidate, true
			break
		}
	}
	s.mu.RUnlock()
	if !ok {
		return map[string]any{"error": "未找到凭据"}
	}
	result, err := s.fetchQuota(jsonBytes(quotaRequest{StorageJSON: jsonBytes(c), AuthID: c.ID}))
	if err != nil {
		return map[string]any{"error": safeError(err)}
	}
	out := result.(map[string]any)
	out["credential"] = map[string]any{"id": c.ID, "label": c.Label, "auth_kind": c.kind()}
	return out
}

func quotaIdentity() any {
	return map[string]any{"identifier": Provider}
}
