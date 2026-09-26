package bridge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type oauthCall struct {
	method string
	url    string
	body   string
	header http.Header
}

// oauthHost 是 OAuth 流程专用的宿主桩：按顺序消费 hostPlan，
// 并记录每次出站请求，便于断言请求体形状。
type oauthHost struct {
	mu      sync.Mutex
	plans   []hostPlan
	streams map[string][]readChunk
	reads   map[string]int
	calls   []oauthCall
	saved   []Credential
}

func newOAuthHost(plans ...hostPlan) *oauthHost {
	return &oauthHost{plans: plans, streams: map[string][]readChunk{}, reads: map[string]int{}}
}

func (h *oauthHost) call(method string, payload, out any) error {
	request, _ := payload.(map[string]any)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "host.http.do_stream":
		body, _ := request["body"].([]byte)
		header, _ := request["headers"].(http.Header)
		h.calls = append(h.calls, oauthCall{method: str(request["method"]), url: str(request["url"]), body: string(body), header: header})
		if len(h.plans) == 0 {
			return fmt.Errorf("unexpected upstream request %q", str(request["url"]))
		}
		plan := h.plans[0]
		h.plans = h.plans[1:]
		if plan.openErr != nil {
			return plan.openErr
		}
		streamID := fmt.Sprintf("oauth-upstream-%d", len(h.calls))
		h.streams[streamID] = plan.chunks
		*out.(*upstreamStream) = upstreamStream{StatusCode: plan.status, Headers: plan.header, StreamID: streamID}
		return nil
	case "host.http.stream_read":
		streamID := str(request["stream_id"])
		chunks, ok := h.streams[streamID]
		if !ok {
			return fmt.Errorf("unknown upstream stream %q", streamID)
		}
		index := h.reads[streamID]
		h.reads[streamID]++
		if index >= len(chunks) {
			*out.(*readChunk) = readChunk{Done: true}
		} else {
			*out.(*readChunk) = chunks[index]
		}
		return nil
	case "host.http.stream_close":
		return nil
	case "host.auth.save":
		raw, ok := request["json"].(json.RawMessage)
		if !ok {
			return errors.New("saved credential was not raw JSON")
		}
		var credential Credential
		if err := json.Unmarshal(raw, &credential); err != nil {
			return err
		}
		h.saved = append(h.saved, credential)
		return nil
	default:
		return fmt.Errorf("unexpected host callback %q", method)
	}
}

func (h *oauthHost) snapshot() ([]oauthCall, []Credential) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]oauthCall(nil), h.calls...), append([]Credential(nil), h.saved...)
}

func (h *oauthHost) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func jsonStatusPlan(status int, body any) hostPlan {
	return hostPlan{status: status, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: jsonBytes(body), Done: true}}}
}

// fakeJWT 构造一个只带 exp 的假 JWT，用于验证过期时间解析。
func fakeJWT(exp time.Time) string {
	encode := func(v any) string { return base64.RawURLEncoding.EncodeToString(jsonBytes(v)) }
	return encode(map[string]string{"alg": "RS256"}) + "." + encode(map[string]float64{"exp": float64(exp.Unix())}) + ".signature"
}

func devicePlan(interval int) hostPlan {
	return jsonStatusPlan(200, map[string]any{
		"device_code":               "device-code-1",
		"user_code":                 "ABCD-EFGH",
		"verification_uri":          "https://authkit.cline.bot/device",
		"verification_uri_complete": "https://authkit.cline.bot/device?user_code=ABCD-EFGH",
		"expires_in":                300,
		"interval":                  interval,
	})
}

func rewindPoll(t *testing.T, s *Service, state string) {
	t.Helper()
	s.oauthMu.Lock()
	session, ok := s.oauth[state]
	if ok {
		session.polledAt = time.Now().Add(-time.Minute)
		s.oauth[state] = session
	}
	s.oauthMu.Unlock()
	if !ok {
		t.Fatalf("login session %q is missing", state)
	}
}

func TestOAuthDeviceLoginIssuesStateAndPersistsCredential(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	expiry := time.Now().Add(40 * time.Minute).Truncate(time.Second)
	accessToken := fakeJWT(expiry)
	h := newOAuthHost(
		devicePlan(5),
		jsonStatusPlan(400, map[string]any{"error": "authorization_pending", "error_description": "pending"}),
		jsonStatusPlan(200, map[string]any{"access_token": accessToken, "refresh_token": "workos-refresh-1", "token_type": "Bearer"}),
		jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{
			"accessToken":  accessToken,
			"tokenType":    "Bearer",
			"expiresAt":    "2026-09-25T02:00:00Z",
			"refreshToken": "cline-refresh-1",
			"userInfo":     map[string]any{"clineUserId": "usr-1", "email": "user@example.com"},
		}}),
	)
	s.SetHost(h.call)

	started, err := s.Handle("auth.login.start", jsonBytes(map[string]any{"Provider": Provider}))
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	start, ok := started.(map[string]any)
	if !ok {
		t.Fatalf("start login returned %#v", started)
	}
	state, _ := start["State"].(string)
	if start["Provider"] != Provider || start["URL"] != "https://authkit.cline.bot/device?user_code=ABCD-EFGH" {
		t.Fatalf("start login response = %#v", start)
	}
	// State 会进入宿主的文件名，必须落在其白名单字符集内。
	if state == "" || len(state) > 128 || strings.Trim(state, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		t.Fatalf("state %q is outside the host whitelist", state)
	}
	if expiresAt, ok := start["ExpiresAt"].(time.Time); !ok || expiresAt.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("start login ExpiresAt = %#v", start["ExpiresAt"])
	}
	calls, _ := h.snapshot()
	if len(calls) != 1 || calls[0].url != workOSAPIBase+workOSDeviceAuthPath || calls[0].method != http.MethodPost {
		t.Fatalf("device authorization call = %#v", calls)
	}
	if !strings.Contains(calls[0].body, "client_id="+workOSClientID) {
		t.Fatalf("device authorization body = %q", calls[0].body)
	}

	// 首次轮询没有历史时间戳，应当直接访问上游并拿到 authorization_pending。
	result, err := s.Handle("auth.login.poll", jsonBytes(map[string]any{"State": state}))
	if err != nil {
		t.Fatalf("poll login: %v", err)
	}
	if poll := result.(map[string]any); poll["Status"] != "pending" {
		t.Fatalf("authorization_pending poll = %#v", poll)
	}
	if h.callCount() != 2 {
		t.Fatalf("first poll should reach the upstream: %d calls", h.callCount())
	}

	// 紧随其后的轮询早于设备码下发的最小间隔，必须直接返回 pending 且不发出站请求。
	result, err = s.Handle("auth.login.poll", jsonBytes(map[string]any{"State": state}))
	if err != nil {
		t.Fatalf("poll login: %v", err)
	}
	if poll := result.(map[string]any); poll["Status"] != "pending" {
		t.Fatalf("throttled poll = %#v", poll)
	}
	if h.callCount() != 2 {
		t.Fatalf("throttled poll issued an upstream request: %d", h.callCount())
	}

	rewindPoll(t, s, state)
	result, err = s.Handle("auth.login.poll", jsonBytes(map[string]any{"State": state}))
	if err != nil {
		t.Fatalf("poll login: %v", err)
	}
	poll := result.(map[string]any)
	if poll["Status"] != "success" {
		t.Fatalf("completed poll = %#v", poll)
	}
	auth, ok := poll["Auth"].(map[string]any)
	if !ok {
		t.Fatalf("completed poll Auth = %#v", poll["Auth"])
	}
	if attributes := auth["Attributes"].(map[string]string); attributes["auth_kind"] != AuthKindOAuth {
		t.Fatalf("credential attributes = %#v", auth["Attributes"])
	}
	credentialJSON, ok := auth["StorageJSON"].([]byte)
	if !ok {
		t.Fatalf("credential storage was not bytes: %#v", auth["StorageJSON"])
	}
	var credential Credential
	if err := json.Unmarshal(credentialJSON, &credential); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	if credential.AuthKind != AuthKindOAuth || credential.AccessToken != workOSTokenPrefix+accessToken {
		t.Fatalf("credential = %#v", credential)
	}
	if credential.RefreshToken != "cline-refresh-1" || credential.AccountID != "usr-1" || credential.Label != "user@example.com" {
		t.Fatalf("credential fields = %#v", credential)
	}
	if !credential.ExpiresAt.Equal(expiry) {
		t.Fatalf("credential expiry = %v, want %v (JWT exp must win)", credential.ExpiresAt, expiry)
	}
	calls, saved := h.snapshot()
	if len(saved) != 1 || saved[0].ID != credential.ID {
		t.Fatalf("saved credentials = %#v", saved)
	}
	var pollCall *oauthCall
	for i := range calls {
		if calls[i].url == workOSAPIBase+workOSDeviceTokenPath {
			pollCall = &calls[i]
			break
		}
	}
	if pollCall == nil || !strings.Contains(pollCall.body, "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code") {
		t.Fatalf("device token poll = %#v", pollCall)
	}
	if last := calls[len(calls)-1]; last.url != "https://api.cline.bot/api/v1/auth/register" || last.method != http.MethodPost {
		t.Fatalf("register call = %#v", last)
	} else if !strings.Contains(last.body, `"accessToken":"`+accessToken+`"`) {
		t.Fatalf("register body = %q", last.body)
	}

	// 登录会话在成功后必须被清理：此时已不存在可回拨的时间戳，重复轮询应直接报错。
	s.oauthMu.Lock()
	_, stillThere := s.oauth[state]
	s.oauthMu.Unlock()
	if stillThere {
		t.Fatal("completed login session was not cleared")
	}
	result, err = s.Handle("auth.login.poll", jsonBytes(map[string]any{"State": state}))
	if err != nil {
		t.Fatalf("poll after completion: %v", err)
	}
	if poll := result.(map[string]any); poll["Status"] != "error" {
		t.Fatalf("poll after completion = %#v", poll)
	}
}

func TestOAuthDeviceLoginReportsDeniedAndExpired(t *testing.T) {
	for _, test := range []struct {
		name   string
		code   string
		needle string
	}{
		{"denied", "access_denied", "取消了授权"},
		{"expired", "expired_token", "已过期"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			h := newOAuthHost(devicePlan(5), jsonStatusPlan(400, map[string]any{"error": test.code}))
			s.SetHost(h.call)
			started, err := s.Handle("auth.login.start", jsonBytes(map[string]any{"Provider": Provider}))
			if err != nil {
				t.Fatalf("start login: %v", err)
			}
			state := started.(map[string]any)["State"].(string)
			rewindPoll(t, s, state)
			result, err := s.Handle("auth.login.poll", jsonBytes(map[string]any{"State": state}))
			if err != nil {
				t.Fatalf("poll login: %v", err)
			}
			poll := result.(map[string]any)
			if poll["Status"] != "error" || !strings.Contains(str(poll["Message"]), test.needle) {
				t.Fatalf("poll = %#v, want error containing %q", poll, test.needle)
			}
		})
	}
}

func TestOAuthRefreshRenewsTokenAndSchedulesNextAttempt(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	expiry := time.Now().Add(45 * time.Minute).Truncate(time.Second)
	existing := Credential{
		Type:         Provider,
		ID:           "clinepassbridge-existing",
		Label:        "existing",
		AuthKind:     AuthKindOAuth,
		AccessToken:  workOSTokenPrefix + fakeJWT(time.Now().Add(-time.Minute)),
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": existing.ID + ".json", "RawJSON": jsonBytes(existing),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(jsonStatusPlan(200, map[string]any{"success": true, "data": map[string]any{
		"accessToken":  "renewed-access",
		"expiresAt":    expiry.Format(time.RFC3339),
		"refreshToken": "new-refresh",
		"userInfo":     map[string]any{"clineUserId": "usr-9"},
	}}))
	s.SetHost(h.call)

	result, err := s.Handle("auth.refresh", jsonBytes(map[string]any{"StorageJSON": jsonBytes(existing), "AuthID": existing.ID}))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	refreshed := result.(map[string]any)
	auth := refreshed["Auth"].(map[string]any)
	var credential Credential
	if err := json.Unmarshal(auth["StorageJSON"].([]byte), &credential); err != nil {
		t.Fatalf("decode refreshed credential: %v", err)
	}
	if credential.AccessToken != workOSTokenPrefix+"renewed-access" || credential.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed credential = %#v", credential)
	}
	if credential.AccountID != "usr-9" {
		t.Fatalf("refreshed account id = %q", credential.AccountID)
	}
	next, ok := refreshed["NextRefreshAfter"].(time.Time)
	if !ok || next.After(expiry) || next.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("NextRefreshAfter = %#v, want before %v", refreshed["NextRefreshAfter"], expiry)
	}
	calls, saved := h.snapshot()
	if len(calls) != 1 || calls[0].url != "https://api.cline.bot/api/v1/auth/refresh" {
		t.Fatalf("refresh call = %#v", calls)
	}
	if !strings.Contains(calls[0].body, `"refreshToken":"old-refresh"`) || !strings.Contains(calls[0].body, `"grantType":"refresh_token"`) {
		t.Fatalf("refresh body = %q", calls[0].body)
	}
	if len(saved) != 1 || saved[0].AccessToken != workOSTokenPrefix+"renewed-access" {
		t.Fatalf("persisted credentials = %#v", saved)
	}
}

// 凭据 ID 必须由账号派生：名字里能看出是哪个账号（内置供应商同款风格），
// 且同一账号重复登录会覆盖同一份凭据，而不是每次多出一条。
func TestCredentialIDIsAccountDerivedAndFileSafe(t *testing.T) {
	if got := credentialID("usr-1", "user@example.com"); got != PluginID+"-user@example.com" {
		t.Fatalf("credential id = %q", got)
	}
	for _, tc := range []struct{ accountID, email string }{
		{"usr-01M2W9Y7XBSS4X00YMH9NARQ4E", ""},
		{"usr-1", "a/b\\c:d*e?f"},
		{"usr-1", "   "},
		{"usr-1", "../../etc/passwd"},
	} {
		id := credentialID(tc.accountID, tc.email)
		if id == "" || strings.ContainsAny(id, `/\:*?"<>|`) || strings.Contains(id, "..") {
			t.Fatalf("unsafe credential id %q (from %q / %q)", id, tc.accountID, tc.email)
		}
	}
	// 同一把 key（或同一账号）重复粘贴要落到同一个 ID（覆盖而非新增），不同 key 必须区分开，
	// 且文件名里不能出现明文 key。
	if keyCredentialID("", "", "sk-same") != keyCredentialID("", "", "sk-same") {
		t.Fatal("keyCredentialID must be stable for the same key")
	}
	if keyCredentialID("", "", "sk-same") == keyCredentialID("", "", "sk-other") {
		t.Fatal("keyCredentialID must differ across keys")
	}
	if strings.Contains(keyCredentialID("", "", "sk-secret-value"), "sk-secret-value") {
		t.Fatal("keyCredentialID must not embed the raw key")
	}
	// 能问到账号时按账号命名：名字里带邮箱，且同账号的不同 key 覆盖同一份凭据。
	named := keyCredentialID("usr-1", "user@example.com", "sk-1")
	if !strings.HasPrefix(named, PluginID+"-key-") || !strings.Contains(named, "user@example.com") {
		t.Fatalf("keyCredentialID = %q, 期望带上账号邮箱", named)
	}
	if named != keyCredentialID("usr-1", "user@example.com", "sk-2") {
		t.Fatal("same account must map to the same credential id")
	}
}

// 会话表满时只淘汰最旧的一条：原来整体清空会把其他并发登录的会话一起干掉。
func TestOAuthSessionLimitEvictsOldestOnly(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	base := time.Now()
	for i := 0; i < oauthSessionLimit; i++ {
		state := fmt.Sprintf("state-%d", i)
		s.putOAuthSession(state, oauthSession{expiresAt: base.Add(time.Duration(i+1) * time.Minute)})
	}
	s.putOAuthSession("state-new", oauthSession{expiresAt: base.Add(time.Hour)})

	s.oauthMu.Lock()
	_, hasOldest := s.oauth["state-0"]
	_, hasNewest := s.oauth["state-new"]
	count := len(s.oauth)
	s.oauthMu.Unlock()
	if hasOldest || !hasNewest || count != oauthSessionLimit {
		t.Fatalf("应只淘汰最旧会话：hasOldest=%v hasNewest=%v count=%d", hasOldest, hasNewest, count)
	}
}

// 凭据 ID 现在按账号/key 派生：删除后再重新添加会落到同一个 ID，
// 旧的删除记录必须失效，否则新凭据会被永久拒绝；记录本身也要带 TTL，不能只增不减。
func TestRevokedCredentialStateIsClearedAndBounded(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := Credential{Type: Provider, ID: keyCredentialID("", "", "sk-same-key-123456"), Label: "key", APIKey: "sk-same-key-123456"}

	s.mu.Lock()
	s.markRevokedLocked(credential.ID)
	s.mu.Unlock()
	if !s.isRevoked(credential.ID) {
		t.Fatal("刚删除的凭据应该被拒绝")
	}

	// 宿主重新解析同一个凭据文件（= 用户重新添加）后，旧记录必须失效。
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	if s.isRevoked(credential.ID) {
		t.Fatal("重新添加的凭据不应继续被当作已删除")
	}

	// 过期记录应被忽略。
	s.mu.Lock()
	s.revoked[credential.ID] = time.Now().Add(-2 * revokedTTL)
	s.mu.Unlock()
	if s.isRevoked(credential.ID) {
		t.Fatal("过期的删除记录应被忽略")
	}

	// 凭据 ID 会拼进文件名，路径穿越必须被拒。
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": "../../evil.json",
		"RawJSON": jsonBytes(Credential{Type: Provider, APIKey: "sk-x-123456789"}),
	})); err == nil {
		t.Fatal("带路径穿越的凭据标识必须被拒绝")
	}
}

// 脱敏必须覆盖 OAuth 的 workos:<jwt> 形态：原来字符类里没有 ':'，
// 只会替掉 "Bearer workos"，JWT 主体照样写进日志。
func TestSafeErrorRedactsWorkOSTokens(t *testing.T) {
	jwt := fakeJWT(time.Now().Add(time.Hour))
	got := safeError(errors.New("请求失败：Bearer " + workOSTokenPrefix + jwt))
	if strings.Contains(got, jwt) || strings.Contains(got, "eyJ") {
		t.Fatalf("token leaked into the message: %q", got)
	}
}

func TestAPIKeyCredentialKeepsStaticRefresh(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := Credential{Type: Provider, ID: "clinepassbridge-apikey", Label: "key", APIKey: "sk-test-123456"}
	h := newOAuthHost()
	s.SetHost(h.call)
	result, err := s.Handle("auth.refresh", jsonBytes(map[string]any{"StorageJSON": jsonBytes(credential)}))
	if err != nil {
		t.Fatalf("refresh api key credential: %v", err)
	}
	next := result.(map[string]any)["NextRefreshAfter"].(time.Time)
	if !next.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("api key should not be scheduled for renewal soon: %v", next)
	}
	if h.callCount() != 0 {
		t.Fatalf("api key refresh issued %d upstream requests", h.callCount())
	}
}

func TestBearerTokenFollowsCredentialKind(t *testing.T) {
	oauth := headers(Credential{AuthKind: AuthKindOAuth, AccessToken: workOSTokenPrefix + "jwt"})
	if got := oauth.Get("Authorization"); got != "Bearer "+workOSTokenPrefix+"jwt" {
		t.Fatalf("oauth authorization = %q", got)
	}
	apiKey := headers(Credential{APIKey: "sk-123"})
	if got := apiKey.Get("Authorization"); got != "Bearer sk-123" {
		t.Fatalf("api key authorization = %q", got)
	}
}

func TestOAuthErrorMessagesAndMatchingSignalsStayConsistent(t *testing.T) {
	rules := requestErrorRules()
	if len(rules) != 1 || rules[0].Status != 500 {
		t.Fatalf("request error rules = %#v", rules)
	}
	matched := strings.Join(rules[0].Match, "|")
	for _, signal := range []string{emptyContentMessage, upstreamEmptyContentHint} {
		if !strings.Contains(matched, signal) {
			t.Fatalf("rules %#v do not cover signal %q", rules[0].Match, signal)
		}
	}
	if !isEmptyError(fail(500, emptyContentMessage)) {
		t.Fatal("Chinese empty content message was not recognised")
	}
	if !isEmptyError(errors.New("Cline: empty response content")) {
		t.Fatal("upstream English empty content hint was not recognised")
	}

	// authData 中的匹配规则必须与判定保持一致，否则 CPA 不会按预期停止重试。
	auth := authData(Credential{ID: "c1", APIKey: "sk-1"}, "c1.json").(map[string]any)
	metadata := auth["Metadata"].(map[string]any)
	scoped := metadata["request_scoped_errors"].([]any)
	if len(scoped) != 1 {
		t.Fatalf("scoped rules = %#v", scoped)
	}
	entries := scoped[0].(map[string]any)["match"].([]string)
	if strings.Join(entries, "|") != strings.Join(rules[0].Match, "|") {
		t.Fatalf("authData match %#v differs from requestErrorRules %#v", entries, rules[0].Match)
	}
}

func TestJWTExpiryReadsWorkOSPrefixedToken(t *testing.T) {
	expiry := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	if got := jwtExpiry(workOSTokenPrefix + fakeJWT(expiry)); !got.Equal(expiry) {
		t.Fatalf("prefixed jwt expiry = %v, want %v", got, expiry)
	}
	if got := jwtExpiry("not-a-jwt"); !got.IsZero() {
		t.Fatalf("invalid token expiry = %v, want zero", got)
	}
}
