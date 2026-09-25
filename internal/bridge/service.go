package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var secretPattern = regexp.MustCompile(`(?i)(?:bearer\s+|sk[-_])[a-z0-9_.-]+`)

type Service struct {
	credentialMu  sync.Mutex
	authFiles     map[string]string
	mu            sync.RWMutex
	cfg           Config
	host          HostCall
	logs          []LogEntry
	creds         map[string]Credential
	authDir       string
	loaded        bool
	stopped       bool
	active        sync.WaitGroup
	streams       map[string]struct{}
	revoked       map[string]bool
	stopCh        chan struct{}
	logWriteError string
	oauthMu       sync.Mutex
	oauth         map[string]oauthSession
}

func NewService() *Service {
	return &Service{cfg: defaultConfig(), creds: map[string]Credential{}, authFiles: map[string]string{}, streams: map[string]struct{}{}, revoked: map[string]bool{}, oauth: map[string]oauthSession{}, stopCh: make(chan struct{})}
}
func (s *Service) SetHost(h func(string, any, any) error) { s.mu.Lock(); s.host = h; s.mu.Unlock() }
func (s *Service) call(method string, in, out any) error {
	s.mu.RLock()
	h := s.host
	s.mu.RUnlock()
	if h == nil {
		return errors.New("宿主回调未初始化")
	}
	return h(method, in, out)
}
func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	c.Models = append([]Model{}, c.Models...)
	return c
}
func id() string { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	tmp := path + ".tmp-" + id()
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}
func (s *Service) configure(raw json.RawMessage) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if e := json.Unmarshal(raw, &req); e != nil {
		return e
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if e := yaml.Unmarshal(req.ConfigYAML, &cfg); e != nil {
			return e
		}
	}
	// CPA passes plugin-owned config. State holds UI changes and bounded request metadata, never API keys.
	if b, e := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json")); e == nil {
		if e = json.Unmarshal(b, &cfg); e != nil {
			return e
		}
	}
	if e := cfg.validate(); e != nil {
		return e
	}
	if e := os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	if !s.loaded {
		if b, e := os.ReadFile(filepath.Join(cfg.DataDir, "requests.json")); e == nil {
			_ = json.Unmarshal(b, &s.logs)
		}
		s.loaded = true
	}
	if len(s.logs) > cfg.LogRetention {
		s.logs = s.logs[len(s.logs)-cfg.LogRetention:]
	}
	return nil
}
func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if e := s.configure(raw); e != nil {
			return nil, e
		}
		return registration(), nil
	case "executor.identifier", "auth.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "model.static", "model.for_auth":
		return s.modelRegistration(), nil
	case "auth.parse":
		return s.parseAuth(raw)
	case "auth.login.start":
		return s.startOAuthLogin(raw)
	case "auth.login.poll":
		return s.pollOAuthLogin(raw)
	case "auth.refresh":
		return s.refreshAuth(raw)
	case "quota.identifier":
		return quotaIdentity(), nil
	case "quota.describe":
		return quotaDescribe(), nil
	case "quota.fetch":
		return s.fetchQuota(raw)
	case "quota.reset":
		return quotaUnsupportedReset(), nil
	case "executor.execute", "executor.execute_stream":
		var r ExecutorRequest
		if e := json.Unmarshal(raw, &r); e != nil {
			return nil, e
		}
		if method == "executor.execute_stream" {
			return s.executeStream(r)
		}
		return s.execute(r)
	case "executor.count_tokens":
		return nil, fail(501, "Cline Pass 未提供精确的 token 计数接口")
	case "executor.http_request":
		return nil, fail(400, "请改用 ClinePassBridge 的模型执行器")
	case "management.register":
		return s.registerManagement(raw)
	case "management.handle":
		return s.management(raw)
	case "plugin.shutdown":
		s.mu.Lock()
		if !s.stopped {
			close(s.stopCh)
		}
		s.stopped = true
		streams := make([]string, 0, len(s.streams))
		for stream := range s.streams {
			streams = append(streams, stream)
		}
		s.mu.Unlock()
		for _, stream := range streams {
			s.closeUpstream(stream)
		}
		s.clearOAuthSessions()
		s.active.Wait()
		return map[string]any{}, nil
	default:
		return nil, fail(400, "不支持的插件方法："+method)
	}
}
func (s *Service) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fail(503, "ClinePassBridge 正在关闭")
	}
	s.active.Add(1)
	return nil
}

// Re-save this provider's auth records so CPA's watcher refreshes model registrations.
func (s *Service) refreshRegistrations() error {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	revision := sha256.Sum256(jsonBytes(s.config().Models))
	s.mu.RLock()
	credentials := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		credentials = append(credentials, c)
	}
	s.mu.RUnlock()
	for _, c := range credentials {
		// CPA skips unchanged files. A model revision makes watcher refreshes reliable.
		c.ModelRevision = hex.EncodeToString(revision[:])
		c.RequestScopedErrors = requestErrorRules()
		s.mu.RLock()
		filename := s.authFiles[c.ID]
		s.mu.RUnlock()
		if filename == "" {
			filename = c.ID + ".json"
		}
		if e := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(c))}, nil); e != nil {
			return e
		}
	}
	return nil
}

func registration() any {
	return map[string]any{"schema_version": 6, "metadata": map[string]any{"Name": "ClinePassBridge", "Version": Version, "Author": "xiao-qiu-qiu", "GitHubRepository": "https://github.com/xiao-qiu-qiu/ClinePassBridge", "Description": "Cline Pass 订阅接入插件：支持账号登录与 API key、可靠的流式转发，并记录用量与实际上游", "ConfigFields": []map[string]any{{"Name": "data_dir", "Type": "string", "Description": "插件状态持久化目录"}}}, "capabilities": map[string]any{"auth_provider": true, "model_provider": true, "executor": true, "executor_model_scope": "both", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}, "management_api": true, "quota_provider": true}}
}
func (s *Service) modelRegistration() any {
	cfg := s.config()
	models := []map[string]any{}
	for _, m := range cfg.Models {
		models = append(models, map[string]any{"ID": m.ID, "Name": m.UpstreamID, "Object": "model", "OwnedBy": Provider, "DisplayName": m.ID, "SupportedGenerationMethods": []string{"chat"}, "UserDefined": true})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}
func (s *Service) resolveModel(model string) (string, error) {
	cfg := s.config()
	for _, m := range cfg.Models {
		if model == m.ID || model == m.UpstreamID {
			return m.UpstreamID, nil
		}
	}
	return "", fail(400, "ClinePassBridge 未启用该模型："+model)
}
func authData(c Credential, filename string) any {
	// 匹配规则必须与空内容时抛出的文案严格一致，否则 CPA 不会按预期停止重试。
	scoped := []any{}
	for _, rule := range requestErrorRules() {
		scoped = append(scoped, map[string]any{"status": rule.Status, "match": rule.Match, "action": rule.Action})
	}
	return map[string]any{"Provider": Provider, "ID": c.ID, "FileName": filename, "Label": c.Label, "Disabled": c.Disabled, "ProxyURL": c.ProxyURL, "StorageJSON": jsonBytes(c), "Metadata": map[string]any{"type": Provider, "request_scoped_errors": scoped}, "Attributes": map[string]string{"auth_kind": c.kind()}}
}
func (s *Service) parseAuth(raw json.RawMessage) (any, error) {
	var r struct {
		Provider, Path, FileName string
		RawJSON                  []byte
		Host                     struct{ AuthDir string }
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	var c Credential
	if e := json.Unmarshal(r.RawJSON, &c); e != nil || c.Type != Provider {
		return map[string]any{"Handled": false}, nil
	}
	if c.bearerToken() == "" {
		return nil, fail(401, "Cline Pass 凭据缺少可用的令牌")
	}
	if c.kind() == AuthKindOAuth && strings.TrimSpace(c.RefreshToken) == "" {
		return nil, fail(401, "Cline Pass 凭据缺少 refresh_token，无法自动续期")
	}
	if c.ID == "" {
		c.ID = strings.TrimSuffix(r.FileName, ".json")
	}
	if c.Label == "" {
		c.Label = c.ID
	}
	c.RequestScopedErrors = requestErrorRules()
	s.mu.Lock()
	s.creds[c.ID] = c
	if r.FileName != "" {
		s.authFiles[c.ID] = filepath.Base(r.FileName)
	}
	if r.Host.AuthDir != "" {
		s.authDir = r.Host.AuthDir
	}
	s.mu.Unlock()
	return map[string]any{"Handled": true, "Auth": authData(c, r.FileName)}, nil
}
func (s *Service) refreshAuth(raw json.RawMessage) (any, error) {
	var r struct {
		StorageJSON    []byte
		AuthID         string
		HostCallbackID string `json:"host_callback_id"`
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	var c Credential
	if e := json.Unmarshal(r.StorageJSON, &c); e != nil {
		return nil, e
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[r.AuthID]; ok {
		c = current
	}
	filename := s.authFiles[c.ID]
	revoked := s.revoked[c.ID] || s.revoked[r.AuthID]
	s.mu.RUnlock()
	if revoked {
		return nil, fail(401, "Cline Pass 凭据已被删除")
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	if c.kind() == AuthKindOAuth {
		if e := s.begin(); e != nil {
			return nil, e
		}
		defer s.active.Done()
		renewed, e := s.renewOAuthCredential(c, r.HostCallbackID)
		if e != nil {
			return nil, e
		}
		return map[string]any{"Auth": authData(renewed, filename), "NextRefreshAfter": nextRefreshTime(renewed)}, nil
	}
	// API key 是长期静态凭据，无需续期。
	return map[string]any{"Auth": authData(c, filename), "NextRefreshAfter": time.Now().Add(365 * 24 * time.Hour)}, nil
}
func (s *Service) selectedCredential(r ExecutorRequest) (Credential, error) {
	var c Credential
	if len(r.StorageJSON) > 0 {
		_ = json.Unmarshal(r.StorageJSON, &c)
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[r.AuthID]; ok {
		c = current
	}
	revoked := s.revoked[c.ID] || s.revoked[r.AuthID]
	s.mu.RUnlock()
	if c.bearerToken() == "" || c.Disabled || revoked {
		return c, fail(401, "Cline Pass 凭据缺失或已停用")
	}
	return c, nil
}
func (s *Service) appendLog(entry LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.Error = safeError(errors.New(entry.Error))
	s.logs = append(s.logs, entry)
	if len(s.logs) > s.cfg.LogRetention {
		s.logs = s.logs[len(s.logs)-s.cfg.LogRetention:]
	}
	s.logWriteError = safeError(atomicJSON(filepath.Join(s.cfg.DataDir, "requests.json"), s.logs))
}
func (s *Service) credentials() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []map[string]any{}
	for _, c := range s.creds {
		out = append(out, map[string]any{"id": c.ID, "label": c.Label, "enabled": !c.Disabled})
	}
	sort.Slice(out, func(i, j int) bool { return str(out[i]["id"]) < str(out[j]["id"]) })
	return out
}
