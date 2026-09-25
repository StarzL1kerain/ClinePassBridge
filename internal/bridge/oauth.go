package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Cline Pass 的账号登录使用 WorkOS 设备码流程，与 Cline CLI 的 `cline auth` 完全一致。
// 以下协议于 2026-09 通过 Cline CLI 3.0.65 反编译与真实账号实测确认：
//
//  1. 申请设备码 POST {workos}/user_management/authorize/device   form: client_id
//  2. 轮询         POST {workos}/user_management/authenticate      form: grant_type/device_code/client_id
//  3. 换取令牌     POST {base}/auth/register                       json: {accessToken, refreshToken}
//  4. 续期         POST {base}/auth/refresh                        json: {refreshToken, grantType:"refresh_token"}
//
// 鉴权头必须带 workos: 前缀（Authorization: Bearer workos:<jwt>）。裸 JWT 会返回 401，
// 这一点已用同一令牌对照实测确认。第 3、4 步的响应统一为 {success, data:{...}}。
const (
	workOSClientID        = "client_01K3A541FN8TA3EPPHTD2325AR"
	workOSAPIBase         = "https://api.workos.com"
	workOSDeviceAuthPath  = "/user_management/authorize/device"
	workOSDeviceTokenPath = "/user_management/authenticate"
	clineRegisterPath     = "/auth/register"
	clineRefreshPath      = "/auth/refresh"
	deviceCodeGrantType   = "urn:ietf:params:oauth:grant-type:device_code"
	refreshGrantType      = "refresh_token"

	// workOSTokenPrefix 是 Cline 对 WorkOS 令牌的存储标记，发送时必须保留。
	workOSTokenPrefix = "workos:"

	// oauthStatePrefix 必须落在宿主 ValidateOAuthState 的白名单 [a-zA-Z0-9-_.] 内。
	oauthStatePrefix = "clinepass-"

	oauthCallTimeout      = 20 * time.Second
	oauthRefreshMargin    = 5 * time.Minute
	oauthDefaultAccessTTL = time.Hour
	oauthDefaultDeviceTTL = 300 * time.Second
	oauthDefaultInterval  = 5 * time.Second
	oauthIntervalStep     = 5 * time.Second
	oauthSessionLimit     = 32
)

// oauthSession 保存一次设备码登录的中间状态，按 State 索引。
type oauthSession struct {
	deviceCode string
	expiresAt  time.Time
	interval   time.Duration
	polledAt   time.Time
}

// workOSPoll 是一次设备码轮询的结果，ErrorCode 为空表示已拿到令牌。
type workOSPoll struct {
	AccessToken  string
	RefreshToken string
	ErrorCode    string
	ErrorDetail  string
}

// clineOAuthData 对应 Cline 第 3、4 步响应里 data 字段的形状。
type clineOAuthData struct {
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresAt    string `json:"expiresAt"`
	RefreshToken string `json:"refreshToken"`
	UserInfo     struct {
		Subject     string `json:"subject"`
		ClineUserID string `json:"clineUserId"`
		Email       string `json:"email"`
	} `json:"userInfo"`
}

func formHeader() http.Header {
	return http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}, "Accept": []string{"application/json"}}
}

func jsonHeader() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}, "Accept": []string{"application/json"}}
}

func statusOr(status int, fallback int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return fallback
}

func durationOr(seconds float64, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return fallback
}

// hostRequest 通过宿主的流式 HTTP 回调完成一次普通请求。
// 这里刻意复用 host.http.do_stream 而不是 host.http.do：本插件的目录请求
// 已经走通这条路径，避免依赖尚未实证的 host.http.do 响应结构。
func (s *Service) hostRequest(callbackID, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	payload := map[string]any{"method": method, "url": rawURL, "headers": headers}
	if len(body) > 0 {
		payload["body"] = body
	}
	if callbackID != "" {
		payload["host_callback_id"] = callbackID
	}
	up, err := s.openUpstream(payload, time.Now().Add(oauthCallTimeout))
	if err != nil {
		return 0, nil, err
	}
	var buf bytes.Buffer
	if err := s.read(up, func(b []byte) error { buf.Write(b); return nil }); err != nil {
		return up.StatusCode, buf.Bytes(), err
	}
	return up.StatusCode, buf.Bytes(), nil
}

// requestDeviceCode 执行第 1 步：申请设备码。
func (s *Service) requestDeviceCode(callbackID string) (oauthSession, string, string, error) {
	form := url.Values{"client_id": {workOSClientID}}
	status, body, err := s.hostRequest(callbackID, http.MethodPost, workOSAPIBase+workOSDeviceAuthPath, formHeader(), []byte(form.Encode()))
	if err != nil {
		return oauthSession{}, "", "", fail(502, "无法连接 Cline 登录服务："+safeError(err))
	}
	var raw struct {
		DeviceCode              string  `json:"device_code"`
		UserCode                string  `json:"user_code"`
		VerificationURI         string  `json:"verification_uri"`
		VerificationURIComplete string  `json:"verification_uri_complete"`
		ExpiresIn               float64 `json:"expires_in"`
		Interval                float64 `json:"interval"`
		Error                   string  `json:"error"`
		ErrorDescription        string  `json:"error_description"`
	}
	if e := json.Unmarshal(body, &raw); e != nil {
		return oauthSession{}, "", "", fail(502, "Cline 登录服务返回了无效 JSON")
	}
	if status < 200 || status >= 300 {
		detail := strings.TrimSpace(raw.ErrorDescription)
		if detail == "" {
			detail = strings.TrimSpace(raw.Error)
		}
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", status)
		}
		return oauthSession{}, "", "", fail(502, "申请登录设备码失败："+detail)
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || (raw.VerificationURI == "" && raw.VerificationURIComplete == "") {
		return oauthSession{}, "", "", fail(502, "Cline 登录服务返回的设备码信息不完整")
	}
	ttl := durationOr(raw.ExpiresIn, oauthDefaultDeviceTTL)
	target := strings.TrimSpace(raw.VerificationURIComplete)
	if target == "" {
		target = strings.TrimSpace(raw.VerificationURI)
	}
	return oauthSession{
		deviceCode: raw.DeviceCode,
		expiresAt:  time.Now().Add(ttl),
		interval:   durationOr(raw.Interval, oauthDefaultInterval),
	}, target, strings.TrimSpace(raw.UserCode), nil
}

// pollDeviceToken 执行第 2 步：轮询设备码对应的令牌。
func (s *Service) pollDeviceToken(callbackID, deviceCode string) (workOSPoll, error) {
	form := url.Values{
		"grant_type":  {deviceCodeGrantType},
		"device_code": {deviceCode},
		"client_id":   {workOSClientID},
	}
	status, body, err := s.hostRequest(callbackID, http.MethodPost, workOSAPIBase+workOSDeviceTokenPath, formHeader(), []byte(form.Encode()))
	if err != nil {
		return workOSPoll{}, fail(502, "无法连接 Cline 登录服务："+safeError(err))
	}
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if e := json.Unmarshal(body, &raw); e != nil {
		return workOSPoll{}, fail(502, "Cline 登录服务返回了无效 JSON")
	}
	if status >= 200 && status < 300 {
		if raw.AccessToken == "" || raw.RefreshToken == "" {
			return workOSPoll{}, fail(502, "Cline 登录服务未返回完整的令牌")
		}
		return workOSPoll{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken}, nil
	}
	code := strings.TrimSpace(raw.Error)
	if code == "" {
		return workOSPoll{}, fail(statusOr(status, 502), fmt.Sprintf("轮询登录状态失败：HTTP %d", status))
	}
	return workOSPoll{ErrorCode: code, ErrorDetail: strings.TrimSpace(raw.ErrorDescription)}, nil
}

// decodeClineData 解析第 3、4 步共用的 {success, data} 信封。
func (s *Service) decodeClineData(status int, raw []byte, action string) (clineOAuthData, error) {
	var env struct {
		Success bool            `json:"success"`
		Data    clineOAuthData  `json:"data"`
		Error   json.RawMessage `json:"error"`
	}
	if e := json.Unmarshal(raw, &env); e != nil {
		return clineOAuthData{}, fail(502, action+"：Cline 返回了无效 JSON")
	}
	if status < 200 || status >= 300 || !env.Success || strings.TrimSpace(env.Data.AccessToken) == "" {
		return clineOAuthData{}, fail(statusOr(status, 502), action+"："+envelopeMessage(env.Error))
	}
	return env.Data, nil
}

func envelopeMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "Cline 未返回成功状态"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if message := strings.TrimSpace(str(obj["message"])); message != "" {
			return message
		}
		if code := strings.TrimSpace(str(obj["code"])); code != "" {
			return code
		}
	}
	return strings.TrimSpace(string(raw))
}

// registerClineTokens 执行第 3 步：用 WorkOS 令牌换取 Cline 账号令牌。
func (s *Service) registerClineTokens(callbackID, accessToken, refreshToken string) (clineOAuthData, error) {
	body := jsonBytes(map[string]string{"accessToken": accessToken, "refreshToken": refreshToken})
	status, raw, err := s.hostRequest(callbackID, http.MethodPost, s.config().BaseURL+clineRegisterPath, jsonHeader(), body)
	if err != nil {
		return clineOAuthData{}, fail(502, "无法连接 Cline 服务："+safeError(err))
	}
	return s.decodeClineData(status, raw, "换取 Cline 令牌失败")
}

// refreshClineTokens 执行第 4 步：续期。
func (s *Service) refreshClineTokens(callbackID, refreshToken string) (clineOAuthData, error) {
	body := jsonBytes(map[string]string{"refreshToken": refreshToken, "grantType": refreshGrantType})
	status, raw, err := s.hostRequest(callbackID, http.MethodPost, s.config().BaseURL+clineRefreshPath, jsonHeader(), body)
	if err != nil {
		return clineOAuthData{}, fail(502, "无法连接 Cline 服务："+safeError(err))
	}
	return s.decodeClineData(status, raw, "续期 Cline 令牌失败")
}

// jwtExpiry 从 JWT 的 exp 声明解析过期时间。Cline CLI 同样以该声明为准。
func jwtExpiry(token string) time.Time {
	token = strings.TrimPrefix(strings.TrimSpace(token), workOSTokenPrefix)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0)
}

// tokenExpiry 优先取 JWT 的 exp，其次取响应里的 ISO 时间，最后兜底一小时。
func tokenExpiry(token, isoTime string, now time.Time) time.Time {
	if exp := jwtExpiry(token); !exp.IsZero() {
		return exp
	}
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(isoTime)); err == nil {
		return parsed
	}
	return now.Add(oauthDefaultAccessTTL)
}

// newOAuthCredential 依据第 3 步结果构造一条 OAuth 凭据。
func newOAuthCredential(data clineOAuthData) Credential {
	label := strings.TrimSpace(data.UserInfo.Email)
	if label == "" {
		label = "Cline Pass"
	}
	c := Credential{
		Type:                Provider,
		ID:                  PluginID + "-" + id(),
		Label:               label,
		AuthKind:            AuthKindOAuth,
		AccessToken:         workOSTokenPrefix + strings.TrimSpace(data.AccessToken),
		RefreshToken:        strings.TrimSpace(data.RefreshToken),
		AccountID:           strings.TrimSpace(data.UserInfo.ClineUserID),
		RequestScopedErrors: requestErrorRules(),
	}
	c.ExpiresAt = tokenExpiry(c.AccessToken, data.ExpiresAt, time.Now())
	return c
}

// nextRefreshTime 给出宿主最早应再次续期的时间。
func nextRefreshTime(c Credential) time.Time {
	if c.ExpiresAt.IsZero() {
		return time.Now().Add(oauthDefaultAccessTTL - oauthRefreshMargin)
	}
	next := c.ExpiresAt.Add(-oauthRefreshMargin)
	if next.Before(time.Now().Add(time.Minute)) {
		return time.Now().Add(time.Minute)
	}
	return next
}

// ---- 会话表 ----

func (s *Service) putOAuthSession(state string, session oauthSession) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauth == nil {
		s.oauth = map[string]oauthSession{}
	}
	now := time.Now()
	for key, item := range s.oauth {
		if now.After(item.expiresAt) {
			delete(s.oauth, key)
		}
	}
	if len(s.oauth) >= oauthSessionLimit {
		s.oauth = map[string]oauthSession{}
	}
	s.oauth[state] = session
}

func (s *Service) oauthSession(state string) (oauthSession, bool) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	session, ok := s.oauth[state]
	if !ok || time.Now().After(session.expiresAt) {
		return oauthSession{}, false
	}
	return session, true
}

func (s *Service) dropOAuthSession(state string) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	delete(s.oauth, state)
}

func (s *Service) clearOAuthSessions() {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	s.oauth = map[string]oauthSession{}
}

// ---- 宿主登录入口 ----

func loginPending(message string) map[string]any {
	return map[string]any{"Status": "pending", "Message": message}
}

func loginError(message string) map[string]any {
	return map[string]any{"Status": "error", "Message": message}
}

// startOAuthLogin 响应 auth.login.start：返回设备码登录 URL 与轮询用的 State。
func (s *Service) startOAuthLogin(raw json.RawMessage) (any, error) {
	var r struct {
		Provider string
		BaseURL  string
		Host     struct{ AuthDir string }
		Metadata map[string]any `json:"Metadata"`
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.Host.AuthDir != "" {
		s.mu.Lock()
		s.authDir = r.Host.AuthDir
		s.mu.Unlock()
	}
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	session, target, userCode, err := s.requestDeviceCode("")
	if err != nil {
		return nil, err
	}
	state := oauthStatePrefix + id()
	s.putOAuthSession(state, session)
	response := map[string]any{
		"Provider":  Provider,
		"URL":       target,
		"State":     state,
		"ExpiresAt": session.expiresAt,
	}
	if userCode != "" {
		// 部分宿主会渲染该字段；URL 里已自带验证码，缺失也不影响登录。
		response["Metadata"] = map[string]any{"user_code": userCode}
	}
	return response, nil
}

// pollOAuthLogin 响应 auth.login.poll：轮询设备码，成功后返回可直接落盘的凭据。
func (s *Service) pollOAuthLogin(raw json.RawMessage) (any, error) {
	var r struct {
		Provider string
		State    string
		Host     struct{ AuthDir string }
		Metadata map[string]any `json:"Metadata"`
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	state := strings.TrimSpace(r.State)
	if state == "" {
		return loginError("登录会话标识为空，请重新发起登录"), nil
	}
	session, ok := s.oauthSession(state)
	if !ok {
		return loginError("登录会话不存在或已过期，请重新发起登录"), nil
	}
	now := time.Now()
	// 设备码接口有最小轮询间隔，宿主可能询问得更频繁。
	if !session.polledAt.IsZero() && now.Sub(session.polledAt) < session.interval {
		return loginPending("等待浏览器完成授权"), nil
	}
	session.polledAt = now
	s.putOAuthSession(state, session)

	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	poll, err := s.pollDeviceToken("", session.deviceCode)
	if err != nil {
		return nil, err
	}
	switch poll.ErrorCode {
	case "":
	case "authorization_pending":
		return loginPending("等待浏览器完成授权"), nil
	case "slow_down":
		session.interval += oauthIntervalStep
		s.putOAuthSession(state, session)
		return loginPending("等待浏览器完成授权"), nil
	case "access_denied":
		s.dropOAuthSession(state)
		return loginError("你在浏览器中取消了授权，请重新发起登录"), nil
	case "expired_token":
		s.dropOAuthSession(state)
		return loginError("登录设备码已过期，请重新发起登录"), nil
	default:
		s.dropOAuthSession(state)
		detail := poll.ErrorDetail
		if detail == "" {
			detail = poll.ErrorCode
		}
		return loginError("登录失败：" + detail), nil
	}

	data, err := s.registerClineTokens("", poll.AccessToken, poll.RefreshToken)
	if err != nil {
		return nil, err
	}
	credential := newOAuthCredential(data)
	filename, err := s.persistCredential(credential)
	if err != nil {
		return nil, err
	}
	s.dropOAuthSession(state)
	message := "登录成功"
	if data.UserInfo.Email != "" {
		message = "登录成功：" + data.UserInfo.Email
	}
	return map[string]any{"Status": "success", "Message": message, "Auth": authData(credential, filename)}, nil
}

// persistCredential 把凭据写入 CPA 的 auth 目录并同步内存状态。
func (s *Service) persistCredential(c Credential) (string, error) {
	filename := c.ID + ".json"
	var saved struct {
		Path string `json:"path"`
	}
	if e := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(c))}, &saved); e != nil {
		return filename, fail(500, "凭据保存失败")
	}
	s.mu.Lock()
	s.creds[c.ID] = c
	s.authFiles[c.ID] = filename
	if saved.Path != "" {
		s.authDir = filepath.Dir(saved.Path)
	}
	s.mu.Unlock()
	return filename, nil
}

func (s *Service) authFileFor(credentialID string) string {
	s.mu.RLock()
	filename := s.authFiles[credentialID]
	s.mu.RUnlock()
	if filename == "" {
		filename = credentialID + ".json"
	}
	return filename
}

// renewOAuthCredential 续期一条 OAuth 凭据，成功后回写 auth 文件与内存。
func (s *Service) renewOAuthCredential(c Credential, callbackID string) (Credential, error) {
	if c.kind() != AuthKindOAuth || strings.TrimSpace(c.RefreshToken) == "" {
		return c, fail(500, "该凭据缺少 refresh_token，无法续期")
	}
	data, err := s.refreshClineTokens(callbackID, c.RefreshToken)
	if err != nil {
		return c, err
	}
	renewed := c
	renewed.AccessToken = workOSTokenPrefix + strings.TrimSpace(data.AccessToken)
	// 实测上游不会轮换 refresh_token；若将来返回新值，以新值为准。
	if value := strings.TrimSpace(data.RefreshToken); value != "" {
		renewed.RefreshToken = value
	}
	if value := strings.TrimSpace(data.UserInfo.ClineUserID); value != "" {
		renewed.AccountID = value
	}
	renewed.ExpiresAt = tokenExpiry(renewed.AccessToken, data.ExpiresAt, time.Now())
	renewed.RequestScopedErrors = requestErrorRules()
	filename := s.authFileFor(c.ID)
	if e := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(renewed))}, nil); e != nil {
		return c, fail(500, "续期后的凭据保存失败")
	}
	s.mu.Lock()
	s.creds[c.ID] = renewed
	s.mu.Unlock()
	return renewed, nil
}

// renewIfNeeded 在令牌进入续期窗口时先行续期。
// 续期失败时不阻断请求：当前令牌可能仍在有效期内，真实失败会以上游错误记录到请求日志。
func (s *Service) renewIfNeeded(c Credential, callbackID string) Credential {
	if !c.needsRefresh(time.Now()) {
		return c
	}
	renewed, err := s.renewOAuthCredential(c, callbackID)
	if err != nil {
		return c
	}
	return renewed
}
