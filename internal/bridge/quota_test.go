package bridge

import (
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func futureTime() time.Time { return time.Now().Add(time.Hour) }

func tempDataDir(t *testing.T) string {
	t.Helper()
	return filepath.ToSlash(t.TempDir())
}

func oauthCredential(accountID string) Credential {
	return Credential{
		Type:         Provider,
		ID:           "clinepassbridge-quota",
		Label:        "user@example.com",
		AuthKind:     AuthKindOAuth,
		AccessToken:  workOSTokenPrefix + "jwt-value",
		RefreshToken: "refresh-value",
		AccountID:    accountID,
		ExpiresAt:    futureTime(),
	}
}

func planResponse() hostPlan {
	return jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{
		"plan": map[string]any{
			"name":        "Cline Pass (Monthly)[Internal]",
			"displayName": "Cline Pass (Monthly)",
			"interval":    "Monthly",
			"isActive":    true,
			"entitlements": map[string]any{"cline_pass": map[string]any{
				"enabled": true,
				"inferenceCapThreshold": map[string]any{
					"last5HoursUsageCostUSDPerUser": 1000000000,
					"last7daysUsageCostUSDPerUser":  2500000000,
					"last30daysUsageCostUSDPerUser": 5000000000,
				},
			}},
		},
		"currentPeriodStart": "2026-09-19T07:55:10Z",
		"currentPeriodEnd":   "2026-10-19T07:55:10Z",
	}})
}

func balanceResponse(balance float64) hostPlan {
	return jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{"userId": "usr-1", "balance": balance}})
}

// usageLimitsResponse 复刻上游 /users/me/plan/usage-limits 的真实形状。
func usageLimitsResponse() hostPlan {
	return jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{"limits": []any{
		map[string]any{"type": "five_hour", "percentUsed": 0},
		map[string]any{"type": "weekly", "percentUsed": 100, "resetsAt": "2026-09-26T08:30:42.334622744Z"},
		map[string]any{"type": "monthly", "percentUsed": 50, "resetsAt": "2026-10-19T08:30:42.336899925Z"},
	}}})
}

func bucketByWindow(t *testing.T, response any, window string) map[string]any {
	t.Helper()
	out, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("quota response = %#v", response)
	}
	groups, ok := out["groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("quota groups = %#v", out["groups"])
	}
	buckets, ok := groups[0].(map[string]any)["buckets"].([]any)
	if !ok {
		t.Fatalf("quota buckets = %#v", groups[0])
	}
	for _, item := range buckets {
		bucket := item.(map[string]any)
		if bucket["window"] == window {
			return bucket
		}
	}
	t.Fatalf("bucket %q not found in %#v", window, buckets)
	return nil
}

func metricByKey(t *testing.T, response any, key string) map[string]any {
	t.Helper()
	out, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("quota response = %#v", response)
	}
	summary, ok := out["summary"].([]any)
	if !ok {
		t.Fatalf("quota summary = %#v", out["summary"])
	}
	for _, item := range summary {
		metric := item.(map[string]any)
		if metric["key"] == key {
			return metric
		}
	}
	t.Fatalf("metric %q not found in %#v", key, summary)
	return nil
}

// refreshPlan 复刻上游 /auth/refresh 的响应（设备码流程第 4 步换票）。
func refreshPlan(accessToken string, expiry time.Time) hostPlan {
	return jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{
		"accessToken":  accessToken,
		"expiresAt":    expiry.Format(time.RFC3339),
		"refreshToken": "rotated-or-not",
		"userInfo":     map[string]any{"clineUserId": "usr-1"},
	}})
}

// 额度路径必须自己续期：access_token 只有 1 小时有效期，此前续期只挂在模型请求路径上
// （executor.prepare），于是长时间不用之后每次刷新额度都拿着过期令牌打上游、固定 401。
func TestQuotaRenewsTokenBeforeFetching(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-1")
	credential.ExpiresAt = time.Now().Add(time.Minute) // 已进入续期窗口
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(
		refreshPlan("renewed-access", time.Now().Add(time.Hour).Truncate(time.Second)),
		planResponse(), usageLimitsResponse(), balanceResponse(0),
	)
	s.SetHost(h.call)
	if _, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("fetch quota: %v", err)
	}
	calls, saved := h.snapshot()
	if len(calls) != 4 || calls[0].url != "https://api.cline.bot/api/v1/auth/refresh" {
		t.Fatalf("额度请求前应先续期，实际调用序列 = %#v", calls)
	}
	for _, call := range calls[1:] {
		if got := call.header.Get("Authorization"); got != "Bearer "+workOSTokenPrefix+"renewed-access" {
			t.Fatalf("%s 未使用续期后的令牌：%q", call.url, got)
		}
	}
	if len(saved) != 1 || saved[0].AccessToken != workOSTokenPrefix+"renewed-access" {
		t.Fatalf("续期后的凭据未落盘：%#v", saved)
	}
}

// 上游 401 时强制续期并重试一次：令牌过期不该等到"下一次模型请求"才被顺手修好。
func TestQuotaRenewsAndRetriesAfterUnauthorized(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-1") // 未进入续期窗口，只可能由 401 触发续期
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(
		jsonStatusPlan(401, map[string]any{"error": "Unauthorized: Please make sure you're using the latest version of Cline and re-authenticate your Cline account."}),
		refreshPlan("renewed-access", time.Now().Add(time.Hour).Truncate(time.Second)),
		planResponse(), usageLimitsResponse(), balanceResponse(0),
	)
	s.SetHost(h.call)
	if _, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("401 后应续期重试并成功：%v", err)
	}
	calls, _ := h.snapshot()
	if len(calls) != 5 || calls[1].url != "https://api.cline.bot/api/v1/auth/refresh" {
		t.Fatalf("期望 401 → 续期 → 重试，实际调用序列 = %#v", calls)
	}
	if got := calls[2].url; got != "https://api.cline.bot/api/v1/users/me/plan" {
		t.Fatalf("重试请求 = %q", got)
	}
	if got := calls[2].header.Get("Authorization"); got != "Bearer "+workOSTokenPrefix+"renewed-access" {
		t.Fatalf("重试未使用新令牌：%q", got)
	}
}

// 续期失败必须给出可操作的提示，并把原因写进日志 ——
// 否则用户只能看到一个上游 401 原文，完全不知道是插件没续上。
func TestQuotaSurfacesRenewalFailureAndLogsIt(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-1")
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(
		jsonStatusPlan(401, map[string]any{"error": "Unauthorized: ... re-authenticate your Cline account."}),
		jsonStatusPlan(401, map[string]any{"success": false, "error": "invalid_grant"}),
	)
	s.SetHost(h.call)
	_, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	}))
	if err == nil || !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("续期失败应给出可操作提示，实际 err = %v", err)
	}
	s.mu.RLock()
	logs := append([]LogEntry(nil), s.logs...)
	s.mu.RUnlock()
	logged := false
	for _, entry := range logs {
		if entry.Status == http.StatusUnauthorized && strings.Contains(entry.Error, "续期失败") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("续期失败未写进日志：%#v", logs)
	}
}

func TestQuotaFetchReportsBalanceAndCapCeilings(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-01M2W9Y7XBSS4X00YMH9NARQ4E")
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(planResponse(), usageLimitsResponse(), balanceResponse(-32221))
	s.SetHost(h.call)

	result, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	}))
	if err != nil {
		t.Fatalf("fetch quota: %v", err)
	}

	// 面板按 buckets 画窗口，宿主只认 remainingFraction，因此这几个断言是显示成功的关键。
	fiveHour := bucketByWindow(t, result, "five_hour")
	if math.Abs(fiveHour["remainingFraction"].(float64)-1) > 1e-9 {
		t.Fatalf("five_hour remainingFraction = %v, want 1", fiveHour["remainingFraction"])
	}
	if _, ok := fiveHour["resetTime"]; ok {
		t.Fatalf("five_hour should omit an absent resetTime: %#v", fiveHour)
	}
	weekly := bucketByWindow(t, result, "seven_day")
	if math.Abs(weekly["remainingFraction"].(float64)) > 1e-9 {
		t.Fatalf("seven_day remainingFraction = %v, want 0", weekly["remainingFraction"])
	}
	if weekly["resetTime"] != "2026-09-26T08:30:42.334622744Z" {
		t.Fatalf("seven_day resetTime = %#v", weekly["resetTime"])
	}
	if !strings.Contains(str(weekly["description"]), "已用 100.0%") {
		t.Fatalf("seven_day description = %#v", weekly["description"])
	}
	monthly := bucketByWindow(t, result, "monthly")
	if math.Abs(monthly["remainingFraction"].(float64)-0.5) > 1e-9 {
		t.Fatalf("monthly remainingFraction = %v, want 0.5", monthly["remainingFraction"])
	}

	balance := metricByKey(t, result, "cline_pass_balance")
	// balance 单位是 1e-6 美元；网页端把 -32221 显示为 Credits: -0.0322。
	if balance["format"] != "currency" || balance["currency"] != "USD" {
		t.Fatalf("balance metric = %#v", balance)
	}
	if math.Abs(balance["value"].(float64)-(-0.032221)) > 1e-9 {
		t.Fatalf("balance value = %v, want -0.032221", balance["value"])
	}
	if balance["label"] != "余额" {
		t.Fatalf("balance label = %#v", balance["label"])
	}
	// 三个窗口上限的单位是 1e-8 美元，换算后应为 10 / 25 / 50 美元。
	for key, want := range map[string]float64{
		"cline_pass_cap_last5HoursUsageCostUSDPerUser": 10,
		"cline_pass_cap_last7daysUsageCostUSDPerUser":  25,
		"cline_pass_cap_last30daysUsageCostUSDPerUser": 50,
	} {
		metric := metricByKey(t, result, key)
		if math.Abs(metric["value"].(float64)-want) > 1e-9 {
			t.Fatalf("%s = %v, want %v", key, metric["value"], want)
		}
	}
	if days := metricByKey(t, result, "cline_pass_period_days_left"); days["value"].(float64) <= 0 {
		t.Fatalf("period days left = %#v", days)
	}
	subscription, ok := result.(map[string]any)["subscription"].(map[string]any)
	if !ok || subscription["plan"] != "Cline Pass (Monthly)" || subscription["tierName"] != "Monthly" {
		t.Fatalf("subscription = %#v", result.(map[string]any)["subscription"])
	}

	calls, _ := h.snapshot()
	if len(calls) != 3 {
		t.Fatalf("quota used %d upstream calls: %#v", len(calls), calls)
	}
	if calls[0].url != "https://api.cline.bot/api/v1/users/me/plan" || calls[0].method != http.MethodGet {
		t.Fatalf("plan call = %#v", calls[0])
	}
	if calls[1].url != "https://api.cline.bot/api/v1/users/me/plan/usage-limits" {
		t.Fatalf("usage-limits call = %#v", calls[1])
	}
	if calls[2].url != "https://api.cline.bot/api/v1/users/usr-01M2W9Y7XBSS4X00YMH9NARQ4E/balance" {
		t.Fatalf("balance call = %#v", calls[2])
	}
	// 额度接口必须沿用带 workos: 前缀的令牌，裸 JWT 会被上游拒绝。
	if got := calls[0].header.Get("Authorization"); got != "Bearer "+workOSTokenPrefix+"jwt-value" {
		t.Fatalf("quota authorization = %q", got)
	}
}

func TestQuotaFetchResolvesAccountWhenCredentialHasNone(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("")
	h := newOAuthHost(
		planResponse(),
		usageLimitsResponse(),
		jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{"id": "usr-7"}}),
		balanceResponse(-1000000),
	)
	s.SetHost(h.call)
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	result, err := s.Handle("quota.fetch", jsonBytes(map[string]any{"auth_id": credential.ID}))
	if err != nil {
		t.Fatalf("fetch quota: %v", err)
	}
	if value := metricByKey(t, result, "cline_pass_balance")["value"].(float64); math.Abs(value-(-1)) > 1e-9 {
		t.Fatalf("balance value = %v, want -1", value)
	}
	calls, _ := h.snapshot()
	if len(calls) != 4 || calls[2].url != "https://api.cline.bot/api/v1/users/me" {
		t.Fatalf("account resolution calls = %#v", calls)
	}
}

func TestQuotaFetchRejectsCredentialWithoutToken(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newOAuthHost()
	s.SetHost(h.call)
	_, err := s.Handle("quota.fetch", jsonBytes(map[string]any{"auth_id": "missing"}))
	if err == nil || statusOf(err) != 401 || !strings.Contains(err.Error(), "凭据") {
		t.Fatalf("missing credential quota = %v (status %d)", err, statusOf(err))
	}
	if h.callCount() != 0 {
		t.Fatalf("missing credential issued %d upstream calls", h.callCount())
	}
}

func TestQuotaDescribeDeclaresProviderWithoutReset(t *testing.T) {
	described, err := NewService().Handle("quota.describe", nil)
	if err != nil {
		t.Fatalf("describe quota: %v", err)
	}
	out := described.(map[string]any)
	// 宿主侧结构体是 snake_case 标签且无自定义反序列化，因此两个键都要给出。
	for _, key := range []string{"supported_providers", "SupportedProviders"} {
		providers, ok := out[key].([]string)
		if !ok || len(providers) != 1 || providers[0] != Provider {
			t.Fatalf("describe %s = %#v", key, out[key])
		}
	}
	for _, key := range []string{"supports_reset", "supportsReset"} {
		if out[key] != false {
			t.Fatalf("describe %s = %#v", key, out[key])
		}
	}
	if out["display_name"] != "Cline Pass 额度" {
		t.Fatalf("describe display_name = %#v", out["display_name"])
	}
	identity, err := NewService().Handle("quota.identifier", nil)
	if err != nil || identity.(map[string]any)["identifier"] != Provider {
		t.Fatalf("quota identifier = %#v, %v", identity, err)
	}
}

func TestQuotaResetIsRejectedInChinese(t *testing.T) {
	result, err := NewService().Handle("quota.reset", nil)
	if err != nil {
		t.Fatalf("reset quota: %v", err)
	}
	out := result.(map[string]any)
	if out["success"] != false || !strings.Contains(str(out["message"]), "未提供") {
		t.Fatalf("quota reset = %#v", out)
	}
}

func TestRegistrationDeclaresQuotaProvider(t *testing.T) {
	registered, err := NewService().Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte("data_dir: " + tempDataDir(t) + "\n")}))
	if err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	capabilities := registered.(map[string]any)["capabilities"].(map[string]any)
	if capabilities["quota_provider"] != true {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}
