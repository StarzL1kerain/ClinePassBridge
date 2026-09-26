package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Version = "0.1.13"
const Provider = "cline-pass"
const PluginID = "clinepassbridge"

// 凭据种类。空值与 AuthKindAPIKey 等价，兼容改造前只保存 api_key 的旧文件。
const (
	AuthKindAPIKey = "api_key"
	AuthKindOAuth  = "oauth"
)

// emptyContentMessage 是插件自己判定“聚合后没有任何输出”时给出的提示。
const emptyContentMessage = "上游返回了空内容"

// upstreamEmptyContentHint 是 Cline 自己返回空内容时的原文提示，会经 errorMessage 透传出来，
// 因此空内容判定与交给 CPA 的 request_scoped_errors.match 都必须同时识别它，否则原生模式的回退会失效。
const upstreamEmptyContentHint = "empty response content"

// emptyContentSignals 返回所有代表“空内容”的信号串。
func emptyContentSignals() []string {
	return []string{emptyContentMessage, upstreamEmptyContentHint}
}

type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string           { return e.Message }
func (e *APIError) StatusCode() int         { return e.Status }
func (e *APIError) Code() string            { return e.Kind }
func fail(status int, message string) error { return &APIError{status, "upstream_error", message} }

type HostCall func(string, any, any) error
type ExecutorRequest struct {
	deadline                                               time.Time
	AuthID, AuthProvider, Model, Format, SourceFormat, Alt string
	Stream                                                 bool
	Headers                                                http.Header
	Query                                                  url.Values
	OriginalRequest, Payload, StorageJSON                  []byte
	Metadata, AuthMetadata                                 map[string]any
	AuthAttributes                                         map[string]string
	StreamID                                               string `json:"stream_id"`
	HostCallbackID                                         string `json:"host_callback_id"`
}
type Response struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any `json:",omitempty"`
}
type ManagementRequest struct {
	Method, Path   string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type Credential struct {
	Type                string             `json:"type"`
	ID                  string             `json:"id"`
	Label               string             `json:"label"`
	APIKey              string             `json:"api_key,omitempty"`
	AuthKind            string             `json:"auth_kind,omitempty"`
	AccessToken         string             `json:"access_token,omitempty"`
	RefreshToken        string             `json:"refresh_token,omitempty"`
	ExpiresAt           time.Time          `json:"expires_at,omitempty"`
	AccountID           string             `json:"account_id,omitempty"`
	Disabled            bool               `json:"disabled"`
	ProxyURL            string             `json:"proxy_url,omitempty"`
	RequestScopedErrors []RequestErrorRule `json:"request_scoped_errors"`
	ModelRevision       string             `json:"model_revision,omitempty"`
}

// kind 归一化凭据种类，空值按 api_key 处理。
func (c Credential) kind() string {
	if strings.TrimSpace(c.AuthKind) == "" {
		return AuthKindAPIKey
	}
	return c.AuthKind
}

// bearerToken 返回直接填入 Authorization: Bearer 的值。
// OAuth 凭据的 AccessToken 已按 Cline 约定带 workos: 前缀，实测缺少该前缀会返回 401。
func (c Credential) bearerToken() string {
	if c.kind() == AuthKindOAuth {
		return strings.TrimSpace(c.AccessToken)
	}
	return strings.TrimSpace(c.APIKey)
}

// needsRefresh 判断 OAuth 令牌是否已进入续期窗口。
func (c Credential) needsRefresh(now time.Time) bool {
	if c.kind() != AuthKindOAuth || c.ExpiresAt.IsZero() || strings.TrimSpace(c.RefreshToken) == "" {
		return false
	}
	return now.Add(oauthRefreshMargin).After(c.ExpiresAt)
}

// credentialID 用账号派生凭据 ID：名字里能直接看出是哪个账号（内置供应商也是这个风格，
// 如 claude-<邮箱>.json），而且同一账号重复登录会覆盖同一份凭据，而不是每次多出一条。
func credentialID(accountID, email string) string {
	if slug := slugifyAccount(email); slug != "" {
		return PluginID + "-" + slug
	}
	if slug := slugifyAccount(accountID); slug != "" {
		return PluginID + "-" + slug
	}
	return PluginID + "-" + id()
}

// keyCredentialID 用账号派生稳定 ID：命名成 key-<邮箱>，一眼看出是哪个账号的 key，
// 也让同一账号重复粘贴覆盖同一份凭据（与账号登录那套约定一致）。
// 拿不到账号信息时退回 key 的哈希：同一把 key 仍然稳定，且文件名里不出现明文 key。
func keyCredentialID(accountID, email, apiKey string) string {
	if slug := slugifyAccount(email); slug != "" {
		return PluginID + "-key-" + slug
	}
	if slug := slugifyAccount(accountID); slug != "" {
		return PluginID + "-key-" + slug
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return PluginID + "-key-" + hex.EncodeToString(sum[:])[:12]
}

// slugifyAccount 只保留可安全用作文件名的字符（ID 会拼进 auth-dir 的文件名），
// 其余折叠成 '-'；必须与 update/delete 的路径校验保持一致。
func slugifyAccount(value string) string {
	var b strings.Builder
	dashPending := false
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dashPending = false
		case r == '_' || r == '.' || r == '-' || r == '@':
			b.WriteRune(r)
			dashPending = false
		default:
			if !dashPending && b.Len() > 0 {
				b.WriteByte('-')
				dashPending = true
			}
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-.")
}

type RequestErrorRule struct {
	Status int      `json:"status"`
	Match  []string `json:"match"`
	Action string   `json:"action"`
}

func requestErrorRules() []RequestErrorRule {
	return []RequestErrorRule{{Status: 500, Match: emptyContentSignals(), Action: "stop"}}
}

type Model struct {
	ID         string   `json:"id" yaml:"id"`
	UpstreamID string   `json:"upstream_id" yaml:"upstream_id"`
	Providers  []string `json:"providers" yaml:"providers"`
}
type Config struct {
	DataDir          string  `json:"data_dir" yaml:"data_dir"`
	BaseURL          string  `json:"base_url" yaml:"base_url"`
	Models           []Model `json:"models" yaml:"models"`
	NonstreamMode    string  `json:"nonstream_mode" yaml:"nonstream_mode"`
	TimeoutSeconds   int     `json:"timeout_seconds" yaml:"timeout_seconds"`
	LogRetention     int     `json:"log_retention" yaml:"log_retention"`
	MaxResponseBytes int     `json:"max_response_bytes" yaml:"max_response_bytes"`
}

func defaultConfig() Config {
	return Config{DataDir: "plugins/clinepassbridge-data", BaseURL: "https://api.cline.bot/api/v1", Models: []Model{{ID: "deepseek-v4.1-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"}, {ID: "deepseek-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"}, {ID: "cline-pass/deepseek-v4.1-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"}}, NonstreamMode: "stream-aggregate", TimeoutSeconds: 180, LogRetention: 1000, MaxResponseBytes: 16 << 20}
}
func (c *Config) validate() error {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Scheme != "https" || u.Host != "api.cline.bot" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/api/v1" {
		return fail(400, "base_url 必须为 https://api.cline.bot/api/v1")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fail(400, "timeout_seconds 必须在 10 到 1800 之间")
	}
	if c.LogRetention < 50 || c.LogRetention > 10000 {
		return fail(400, "log_retention 必须在 50 到 10000 之间")
	}
	if c.MaxResponseBytes < 65536 || c.MaxResponseBytes > 64<<20 {
		return fail(400, "max_response_bytes 必须在 64 KiB 到 64 MiB 之间")
	}
	if c.NonstreamMode != "native" && c.NonstreamMode != "native-fallback" && c.NonstreamMode != "stream-aggregate" {
		return fail(400, "nonstream_mode 无效")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if strings.TrimSpace(m.ID) == "" || seen[m.ID] {
			return fail(400, "模型别名不能为空且不能重复")
		}
		seen[m.ID] = true
		if strings.TrimSpace(m.UpstreamID) == "" || strings.ContainsAny(m.ID+m.UpstreamID, "\r\n\t") {
			return fail(400, "模型标识不能为空且不能包含控制类空白字符")
		}
	}
	return nil
}

type Attempt struct {
	Status         int    `json:"status"`
	Mode           string `json:"mode"`
	Provider       string `json:"provider"`
	ProviderSource string `json:"provider_source"`
	DurationMS     int64  `json:"duration_ms"`
	Error          string `json:"error,omitempty"`
}
type LogEntry struct {
	ID               string    `json:"id"`
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model"`
	Stream           bool      `json:"stream"`
	Status           int       `json:"status"`
	Provider         string    `json:"provider"`
	ProviderSource   string    `json:"provider_source"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMS           int64     `json:"ttft_ms"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	Credential       string    `json:"credential"`
	Attempts         []Attempt `json:"attempts"`
	Error            string    `json:"error,omitempty"`
}

func jsonBytes(v any) []byte      { b, _ := json.Marshal(v); return b }
func str(v any) string            { s, _ := v.(string); return s }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
func decodeObject(b []byte) (map[string]any, error) {
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil || j == nil {
		return nil, fail(502, "上游返回了无效 JSON")
	}
	return j, nil
}
func errorMessage(j map[string]any) string {
	v := j["error"]
	if m := object(v); m != nil {
		return str(m["message"])
	}
	if s := str(v); s != "" {
		return s
	}
	return "上游请求失败"
}
func statusOf(err error) int {
	if err == nil {
		return 200
	}
	if e, ok := err.(interface{ StatusCode() int }); ok && e.StatusCode() > 0 {
		return e.StatusCode()
	}
	return 502
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// 按字符截断，避免把中文截成半个字符产生非法 UTF-8。
	if runes := []rune(s); len(runes) > 500 {
		s = string(runes[:500])
	}
	return fmt.Sprintf("%s", secretPattern.ReplaceAllString(s, "[已脱敏]"))
}
