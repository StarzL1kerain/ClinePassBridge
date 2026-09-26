package bridge

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed ui/*
var ui embed.FS

const apiBase = "/v0/management/clinepassbridge"

func (s *Service) registerManagement(raw json.RawMessage) (any, error) {
	routes := []map[string]string{}
	for _, p := range []string{"status", "logs", "models", "config", "credentials", "quota"} {
		routes = append(routes, map[string]string{"Method": "GET", "Path": apiBase + "/" + p})
	}
	for _, p := range []string{"models/refresh", "models/test", "credentials"} {
		routes = append(routes, map[string]string{"Method": "POST", "Path": apiBase + "/" + p})
	}
	for _, p := range []string{"models", "config", "credentials"} {
		routes = append(routes, map[string]string{"Method": "PUT", "Path": apiBase + "/" + p})
	}
	routes = append(routes, map[string]string{"Method": "DELETE", "Path": apiBase + "/credentials"})
	return map[string]any{"routes": routes, "resources": []map[string]string{{"Path": "/console", "Menu": "ClinePassBridge", "Description": "Cline Pass 模型、凭据与实际上游日志"}}}, nil
}
func managementJSON(code int, v any) (any, error) {
	return ManagementResponse{StatusCode: code, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: jsonBytes(v)}, nil
}
func (s *Service) management(raw json.RawMessage) (any, error) {
	var r ManagementRequest
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.Method == "GET" && r.Path == "/v0/resource/plugins/"+PluginID+"/console" {
		b, e := ui.ReadFile("ui/index.html")
		if e != nil {
			return nil, e
		}
		b = []byte(strings.ReplaceAll(string(b), "__PASSBRIDGE_API_BASE__", apiBase))
		// 控制台标题栏的 Cline 图标内联成 data URI，避免页面额外发一次请求。
		logo, err := ui.ReadFile("ui/cline-logo.png")
		if err != nil {
			return nil, err
		}
		b = []byte(strings.ReplaceAll(string(b), "__CLINE_LOGO_DATA__", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(logo)))
		authJS, err := ui.ReadFile("ui/cpa-auth.js")
		if err != nil {
			return nil, err
		}
		b = []byte(strings.ReplaceAll(string(b), "/*__CPA_AUTH_COMPAT__*/", string(authJS)))
		return ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}, "X-Content-Type-Options": []string{"nosniff"}}, Body: b}, nil
	}
	if !strings.HasPrefix(r.Path, apiBase+"/") {
		return managementJSON(404, map[string]any{"error": "未找到"})
	}
	p := strings.TrimPrefix(r.Path, apiBase)
	switch r.Method + " " + p {
	case "GET /status":
		s.mu.RLock()
		logError := s.logWriteError
		s.mu.RUnlock()
		return managementJSON(200, map[string]any{"version": Version, "log_persistence_error": logError, "credential_count": len(s.credentials()), "model_count": len(s.config().Models)})
	case "GET /logs":
		return s.logsResponse(r)
	case "GET /config":
		return managementJSON(200, s.config())
	case "PUT /config":
		cfg := s.config()
		dataDir := cfg.DataDir
		base := cfg.BaseURL
		if e := json.Unmarshal(r.Body, &cfg); e != nil {
			return managementJSON(400, map[string]any{"error": "config JSON 无效"})
		}
		cfg.DataDir = dataDir
		cfg.BaseURL = base
		if e := s.saveConfig(cfg); e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, cfg)
	case "GET /models":
		return managementJSON(200, map[string]any{"models": s.config().Models})
	case "PUT /models":
		var in struct {
			Models *[]Model `json:"models"`
		}
		if e := json.Unmarshal(r.Body, &in); e != nil {
			return managementJSON(400, map[string]any{"error": "模型 JSON 无效"})
		}
		cfg := s.config()
		if in.Models == nil {
			return managementJSON(400, map[string]any{"error": "models 必须是数组"})
		}
		// 保留已有的上游元数据与规格（name/description/tags/context_length/max_completion_tokens）：
		// 前端保存时可能只回传 id 与 upstream_id（例如它拿的是"刷新自动补规格之前"的快照），
		// 不保留的话，刚自动填好的规格会被这一次保存原样覆盖掉。
		previous := map[string]Model{}
		for _, m := range cfg.Models {
			previous[m.UpstreamID] = m
		}
		next := *in.Models
		for i := range next {
			old, ok := previous[next[i].UpstreamID]
			if !ok {
				continue
			}
			if next[i].Name == "" {
				next[i].Name = old.Name
			}
			if next[i].Description == "" {
				next[i].Description = old.Description
			}
			if len(next[i].Tags) == 0 {
				next[i].Tags = old.Tags
			}
			if next[i].ContextLength == 0 {
				next[i].ContextLength = old.ContextLength
			}
			if next[i].MaxCompletionTokens == 0 {
				next[i].MaxCompletionTokens = old.MaxCompletionTokens
			}
			if len(next[i].Providers) == 0 {
				next[i].Providers = old.Providers
			}
		}
		cfg.Models = next
		if e := s.saveConfig(cfg); e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, map[string]any{"models": cfg.Models, "message": "已保存，CPA 的凭据注册已刷新。"})
	case "POST /models/refresh":
		models, e := s.refreshModels(r.HostCallbackID)
		if e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, map[string]any{"models": models})
	case "POST /models/test":
		return s.testModel(r)
	case "GET /credentials":
		return managementJSON(200, map[string]any{"items": s.credentials()})
	case "GET /quota":
		return managementJSON(200, s.managementQuota(r.Query.Get("id")))
	case "POST /credentials":
		return s.importCredential(r)
	case "PUT /credentials":
		return s.updateCredential(r)
	case "DELETE /credentials":
		return s.deleteCredential(r.Query.Get("id"))
	default:
		return managementJSON(404, map[string]any{"error": "未找到"})
	}
}
func (s *Service) saveConfig(cfg Config) error {
	if e := cfg.validate(); e != nil {
		return e
	}
	if e := atomicJSON(filepath.Join(cfg.DataDir, "settings.json"), cfg); e != nil {
		return e
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return s.refreshRegistrations()
}
func (s *Service) logsResponse(r ManagementRequest) (any, error) {
	limit, _ := strconv.Atoi(r.Query.Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.Query.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	search := strings.ToLower(r.Query.Get("search"))
	status := r.Query.Get("status")
	provider := r.Query.Get("provider")
	s.mu.RLock()
	defer s.mu.RUnlock()
	filtered := []LogEntry{}
	for i := len(s.logs) - 1; i >= 0; i-- {
		v := s.logs[i]
		if search != "" && !strings.Contains(strings.ToLower(v.ID+" "+v.Model+" "+v.UpstreamModel+" "+v.Provider+" "+v.Error), search) {
			continue
		}
		if provider != "" && provider != "all" && !strings.EqualFold(provider, v.Provider) {
			continue
		}
		if status != "" && status != "all" {
			if status == "success" && v.Status >= 400 || status == "error" && v.Status < 400 {
				continue
			}
			if n, e := strconv.Atoi(status); e == nil && n != v.Status {
				continue
			}
		}
		filtered = append(filtered, v)
	}
	total := len(filtered)
	var prompt, completion, cached int64
	for _, entry := range filtered {
		prompt += entry.PromptTokens
		completion += entry.CompletionTokens
		cached += entry.CachedTokens
	}
	var cacheRate any
	if prompt > 0 {
		cacheRate = float64(cached) / float64(prompt)
	}
	summary := map[string]any{"requests": total, "prompt_tokens": prompt, "completion_tokens": completion, "cached_tokens": cached, "cache_rate": cacheRate}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return managementJSON(200, map[string]any{"items": filtered[offset:end], "total": total, "summary": summary})
}
func (s *Service) importCredential(r ManagementRequest) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	var in struct {
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "凭据 JSON 无效"})
	}
	in.APIKey = strings.TrimSpace(in.APIKey)
	if len(in.APIKey) < 8 || strings.ContainsAny(in.APIKey, "\r\n") {
		return managementJSON(400, map[string]any{"error": "API key 无效"})
	}
	if len(in.Label) > 100 {
		return managementJSON(400, map[string]any{"error": "备注长度不能超过 100 个字符"})
	}
	// 先用这把 key 问一次上游账号：既好把凭据命名成 key-<邮箱>（与账号登录那套命名一致），
	// 也能在导入时就发现"这把 key 根本无效"——仪表盘上的 key 只在创建时完整显示一次，
	// 之后复制到的是掩码值，粘贴时若不拦住，会变成一条看似正常、其实永远 401 的凭据。
	accountID, email, accountErr := s.resolveKeyAccount(r.HostCallbackID, in.APIKey)
	if code := statusOf(accountErr); code == http.StatusUnauthorized || code == http.StatusForbidden {
		return managementJSON(code, map[string]any{"error": "这把 API key 被上游拒绝：可能复制到的是页面上的掩码值，或该 key 已被撤销。请在 app.cline.bot → Settings → API Keys 重新生成，并在创建后立刻复制完整值"})
	}
	c := Credential{Type: Provider, ID: keyCredentialID(accountID, email, in.APIKey), Label: strings.TrimSpace(in.Label), APIKey: in.APIKey, AccountID: accountID, RequestScopedErrors: requestErrorRules()}
	if c.Label == "" {
		c.Label = email
	}
	if c.Label == "" {
		c.Label = "Cline Pass"
	}
	var saved struct {
		Path string `json:"path"`
	}
	if e := s.call("host.auth.save", map[string]any{"name": c.ID + ".json", "json": json.RawMessage(jsonBytes(c))}, &saved); e != nil {
		return managementJSON(500, map[string]any{"error": "凭据保存失败"})
	}
	s.mu.Lock()
	s.creds[c.ID] = c
	s.authFiles[c.ID] = c.ID + ".json"
	// 同一把 key 重新粘贴会落到同一个 ID，旧的删除记录必须失效。
	delete(s.revoked, c.ID)
	if saved.Path != "" {
		s.authDir = filepath.Dir(saved.Path)
	}
	s.mu.Unlock()
	return managementJSON(201, map[string]any{"id": c.ID, "label": c.Label, "enabled": true})
}
func (s *Service) updateCredential(r ManagementRequest) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	var in struct {
		Label  *string `json:"label"`
		APIKey *string `json:"api_key"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "凭据 JSON 无效"})
	}
	id := r.Query.Get("id")
	s.mu.RLock()
	c, ok := s.creds[id]
	filename := s.authFiles[id]
	s.mu.RUnlock()
	if !ok {
		return managementJSON(404, map[string]any{"error": "未找到凭据"})
	}
	if in.Label != nil {
		c.Label = strings.TrimSpace(*in.Label)
		if len(c.Label) > 100 {
			return managementJSON(400, map[string]any{"error": "备注长度不能超过 100 个字符"})
		}
		if c.Label == "" {
			c.Label = "Cline Pass"
		}
	}
	if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
		key := strings.TrimSpace(*in.APIKey)
		if len(key) < 8 || strings.ContainsAny(key, "\r\n") {
			return managementJSON(400, map[string]any{"error": "API key 无效"})
		}
		c.APIKey = key
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\") {
		return managementJSON(400, map[string]any{"error": "凭据文件名无效"})
	}
	c.RequestScopedErrors = requestErrorRules()
	if e := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(c))}, nil); e != nil {
		return managementJSON(500, map[string]any{"error": "凭据保存失败"})
	}
	s.mu.Lock()
	s.creds[id] = c
	s.mu.Unlock()
	return managementJSON(200, map[string]any{"id": c.ID, "label": c.Label, "enabled": !c.Disabled})
}
func (s *Service) deleteCredential(credentialID string) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[credentialID]
	if !ok {
		return managementJSON(404, map[string]any{"error": "未找到凭据"})
	}
	if s.authDir == "" {
		return managementJSON(409, map[string]any{"error": "尚未解析到凭据存储目录"})
	}
	if filepath.Base(c.ID) != c.ID || strings.ContainsAny(c.ID, "/\\") {
		return managementJSON(400, map[string]any{"error": "凭据 ID 无效"})
	}
	filename := s.authFiles[c.ID]
	if filename == "" {
		filename = c.ID + ".json"
	}
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\") {
		return managementJSON(400, map[string]any{"error": "凭据文件名无效"})
	}
	path := filepath.Join(s.authDir, filename)
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		// 文件已经不在（多半是在宿主的认证文件页里删过）。此时内存里的记录会变成
		// 删不掉的幽灵记录：列表里还显示，删除又卡在这里。删除应当是幂等的。
		s.forgetCredentialLocked(credentialID)
		return managementJSON(200, map[string]any{"deleted": true, "alreadyMissing": true})
	}
	if e != nil {
		return managementJSON(500, map[string]any{"error": "凭据文件读取失败：" + safeError(e)})
	}
	var disk Credential
	if json.Unmarshal(b, &disk) != nil || disk.Type != Provider || disk.ID != c.ID {
		return managementJSON(409, map[string]any{"error": "凭据文件归属校验失败"})
	}
	if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
		return managementJSON(500, map[string]any{"error": "凭据删除失败"})
	}
	s.forgetCredentialLocked(credentialID)
	return managementJSON(200, map[string]any{"deleted": true})
}

// forgetCredentialLocked 把凭据从内存状态里彻底移除，并记下删除记录。
// 调用方必须持有 s.mu。
func (s *Service) forgetCredentialLocked(credentialID string) {
	delete(s.creds, credentialID)
	delete(s.authFiles, credentialID)
	s.markRevokedLocked(credentialID)
}
func (s *Service) refreshModels(callbackID string) ([]Model, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	// The ordinary /models catalog is BYOK. Pass offers live in clinePass here.
	up, e := s.openUpstream(map[string]any{"host_callback_id": callbackID, "method": "GET", "url": s.config().BaseURL + "/ai/cline/recommended-models"}, time.Time{})
	if e != nil {
		return nil, e
	}
	b, e := s.readJSON(up)
	if e != nil {
		return nil, e
	}
	j, e := decodeObject(b)
	if e != nil {
		return nil, e
	}
	candidates := list(j["clinePass"])
	if len(candidates) == 0 {
		return nil, fail(502, "Cline 目录未返回 clinePass 条目")
	}
	models := []Model{}
	seen := map[string]bool{}
	for _, v := range candidates {
		m := object(v)
		modelID := str(m["id"])
		if !strings.HasPrefix(modelID, "cline-pass/") || seen[modelID] {
			continue
		}
		if strings.TrimPrefix(modelID, "cline-pass/") == "" || strings.ContainsAny(modelID, "\r\n\t") {
			continue
		}
		// 上游条目除 id 外还带 name/description/tags，把它们一起接住（description 里就是模型规格）。
		candidate := Model{
			ID:          strings.TrimPrefix(modelID, "cline-pass/"),
			UpstreamID:  modelID,
			Name:        strings.TrimSpace(str(m["name"])),
			Description: strings.TrimSpace(str(m["description"])),
		}
		for _, tag := range list(m["tags"]) {
			if value := strings.TrimSpace(str(tag)); value != "" {
				candidate.Tags = append(candidate.Tags, value)
			}
		}
		models = append(models, candidate)
		seen[modelID] = true
	}
	// 规格自动填充：上游更完整的目录（/ai/cline/models）里有 context_length 与
	// top_provider.max_completion_tokens，按"名字最后一段"匹配后自动补上 —— 不用手填。
	specs := s.fetchModelSpecs(callbackID)
	for i := range models {
		applyModelSpec(&models[i], specs)
	}
	// 已保存的映射里空着的规格也顺手补上（只填空值，不覆盖用户填过的），
	// 并存盘触发宿主重新注册，这样宿主侧立刻能看到规格。
	cfg := s.config()
	changed := false
	for i := range cfg.Models {
		if applyModelSpec(&cfg.Models[i], specs) {
			changed = true
		}
	}
	if changed {
		if e := s.saveConfig(cfg); e != nil {
			s.appendLog(LogEntry{ID: id(), Time: time.Now().UTC(), Model: "(" + PluginID + " 模型规格)", Status: 410,
				Error: "自动补全模型规格后保存失败（已保存的映射未更新）：" + safeError(e)})
		}
	}
	return models, nil
}

// applyModelSpec 按"模型名最后一段"取出规格，只填空值；返回是否发生了改动。
func applyModelSpec(m *Model, specs map[string]clineModelSpec) bool {
	spec, ok := specs[tailName(m.UpstreamID)]
	if !ok {
		return false
	}
	changed := false
	if m.ContextLength == 0 && spec.ContextLength > 0 {
		m.ContextLength = spec.ContextLength
		changed = true
	}
	if m.MaxCompletionTokens == 0 && spec.MaxCompletionTokens > 0 {
		m.MaxCompletionTokens = spec.MaxCompletionTokens
		changed = true
	}
	return changed
}

// tailName 取模型 id 的最后一段：cline-pass/deepseek-v4.1-flash → deepseek-v4.1-flash。
// 上游两个目录的命名不同（deepseek/deepseek-v4.1-flash ↔ cline-pass/deepseek-v4.1-flash），
// 只能靠这一段对齐。
func tailName(id string) string {
	if i := strings.LastIndex(strings.TrimSpace(id), "/"); i >= 0 {
		return strings.TrimSpace(id)[i+1:]
	}
	return strings.TrimSpace(id)
}

// fetchModelSpecs 拉取上游完整模型目录并建规格索引。失败只记一条日志、返回空表：
// 规格是展示增强项，不能因为这一次拉取失败让整个"获取上游模型"失败。
func (s *Service) fetchModelSpecs(callbackID string) map[string]clineModelSpec {
	out := map[string]clineModelSpec{}
	// 带上任一可用凭据的令牌：Pass 商品目录是公开的，但规格目录不保证匿名可读。
	headers := http.Header{"Accept": []string{"application/json"}}
	s.mu.RLock()
	for _, c := range s.creds {
		if token := c.bearerToken(); token != "" {
			headers.Set("Authorization", "Bearer "+token)
			break
		}
	}
	s.mu.RUnlock()
	up, e := s.openUpstream(map[string]any{"host_callback_id": callbackID, "method": "GET", "headers": headers, "url": s.config().BaseURL + "/ai/cline/models"}, time.Time{})
	if e != nil {
		s.appendLog(LogEntry{ID: id(), Time: time.Now().UTC(), Model: "(" + PluginID + " 模型规格)", Status: 410,
			Error: "读取上游模型规格失败（不影响刷新与请求）：" + safeError(e)})
		return out
	}
	b, e := s.readJSON(up)
	if e != nil {
		return out
	}
	j, e := decodeObject(b)
	if e != nil {
		return out
	}
	items := list(j["data"])
	if len(items) == 0 {
		items = list(j["models"])
	}
	for _, v := range items {
		m := object(v)
		id := str(m["id"])
		key := tailName(id)
		if key == "" {
			continue
		}
		var spec clineModelSpec
		if f, ok := m["context_length"].(float64); ok && f > 0 {
			spec.ContextLength = int(f)
		}
		if top := object(m["top_provider"]); len(top) > 0 {
			if f, ok := top["max_completion_tokens"].(float64); ok && f > 0 {
				spec.MaxCompletionTokens = int(f)
			}
			if spec.ContextLength == 0 {
				if f, ok := top["context_length"].(float64); ok && f > 0 {
					spec.ContextLength = int(f)
				}
			}
		}
		if spec.ContextLength > 0 || spec.MaxCompletionTokens > 0 {
			out[key] = spec
		}
	}
	return out
}

// clineModelSpec 是上游目录里能拿到的模型规格。
type clineModelSpec struct {
	ContextLength       int
	MaxCompletionTokens int
}
