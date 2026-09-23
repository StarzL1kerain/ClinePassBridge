package bridge

import (
	"embed"
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
	for _, p := range []string{"status", "logs", "models", "config", "credentials"} {
		routes = append(routes, map[string]string{"Method": "GET", "Path": apiBase + "/" + p})
	}
	for _, p := range []string{"models/refresh", "credentials"} {
		routes = append(routes, map[string]string{"Method": "POST", "Path": apiBase + "/" + p})
	}
	for _, p := range []string{"models", "config"} {
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
		return ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}, "X-Content-Type-Options": []string{"nosniff"}}, Body: b}, nil
	}
	if !strings.HasPrefix(r.Path, apiBase+"/") {
		return managementJSON(404, map[string]any{"error": "not found"})
	}
	p := strings.TrimPrefix(r.Path, apiBase)
	switch r.Method + " " + p {
	case "GET /status":
		s.mu.RLock()
		logError := s.logWriteError
		s.mu.RUnlock()
		return managementJSON(200, map[string]any{"version": Version, "log_persistence_error": logError, "credential_count": len(s.credentials()), "model_count": len(s.config().Models), "pinning": map[string]any{"mode": "automatic", "provider": "", "verified": false, "available": false, "message": "2026-09-24 DeepSeek 对照实测：不存在的 provider 仍成功并路由到 deepseek，当前钉上游参数被忽略"}, "pinning_state": "当前路径已证实忽略钉上游限制，使用自动路由；实际上游来自响应元数据"})
	case "GET /logs":
		return s.logsResponse(r)
	case "GET /config":
		return managementJSON(200, s.config())
	case "PUT /config":
		cfg := s.config()
		dataDir := cfg.DataDir
		base := cfg.BaseURL
		if e := json.Unmarshal(r.Body, &cfg); e != nil {
			return managementJSON(400, map[string]any{"error": "invalid config JSON"})
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
			Models []Model `json:"models"`
		}
		if e := json.Unmarshal(r.Body, &in); e != nil {
			return managementJSON(400, map[string]any{"error": "invalid model JSON"})
		}
		cfg := s.config()
		cfg.Models = in.Models
		if e := s.saveConfig(cfg); e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, map[string]any{"models": cfg.Models, "message": "Saved. CPA credential registrations have been refreshed."})
	case "POST /models/refresh":
		models, e := s.refreshModels(r.HostCallbackID)
		if e != nil {
			return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
		}
		return managementJSON(200, map[string]any{"models": models})
	case "GET /credentials":
		return managementJSON(200, map[string]any{"items": s.credentials()})
	case "POST /credentials":
		return s.importCredential(r)
	case "DELETE /credentials":
		return s.deleteCredential(r.Query.Get("id"))
	default:
		return managementJSON(404, map[string]any{"error": "not found"})
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
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return managementJSON(200, map[string]any{"items": filtered[offset:end], "total": total})
}
func (s *Service) importCredential(r ManagementRequest) (any, error) {
	var in struct {
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil {
		return managementJSON(400, map[string]any{"error": "invalid credential JSON"})
	}
	in.APIKey = strings.TrimSpace(in.APIKey)
	if len(in.APIKey) < 8 || strings.ContainsAny(in.APIKey, "\r\n") {
		return managementJSON(400, map[string]any{"error": "invalid API key"})
	}
	if len(in.Label) > 100 {
		return managementJSON(400, map[string]any{"error": "label exceeds 100 characters"})
	}
	c := Credential{Type: Provider, ID: PluginID + "-" + id(), Label: strings.TrimSpace(in.Label), APIKey: in.APIKey, RequestScopedErrors: requestErrorRules()}
	if c.Label == "" {
		c.Label = "Cline Pass"
	}
	var saved struct {
		Path string `json:"path"`
	}
	if e := s.call("host.auth.save", map[string]any{"name": c.ID + ".json", "json": json.RawMessage(jsonBytes(c))}, &saved); e != nil {
		return managementJSON(500, map[string]any{"error": "credential persistence failed"})
	}
	s.mu.Lock()
	s.creds[c.ID] = c
	if saved.Path != "" {
		s.authDir = filepath.Dir(saved.Path)
	}
	s.mu.Unlock()
	return managementJSON(201, map[string]any{"id": c.ID, "label": c.Label, "enabled": true})
}
func (s *Service) deleteCredential(credentialID string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[credentialID]
	if !ok {
		return managementJSON(404, map[string]any{"error": "credential not found"})
	}
	if s.authDir == "" {
		return managementJSON(409, map[string]any{"error": "credential storage directory not resolved"})
	}
	if filepath.Base(c.ID) != c.ID || strings.ContainsAny(c.ID, "/\\") {
		return managementJSON(400, map[string]any{"error": "invalid credential ID"})
	}
	path := filepath.Join(s.authDir, c.ID+".json")
	b, e := os.ReadFile(path)
	if e != nil {
		return managementJSON(409, map[string]any{"error": "credential file missing"})
	}
	var disk Credential
	if json.Unmarshal(b, &disk) != nil || disk.Type != Provider || disk.ID != c.ID {
		return managementJSON(409, map[string]any{"error": "credential file ownership mismatch"})
	}
	if e = os.Remove(path); e != nil {
		return managementJSON(500, map[string]any{"error": "credential removal failed"})
	}
	delete(s.creds, credentialID)
	s.revoked[credentialID] = true
	return managementJSON(200, map[string]any{"deleted": true})
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
		return nil, fail(502, "Cline catalog returned no clinePass offers")
	}
	cfg := s.config()
	seen := map[string]bool{}
	for _, m := range cfg.Models {
		seen[m.ID] = true
	}
	for _, v := range candidates {
		m := object(v)
		modelID := str(m["id"])
		if !strings.HasPrefix(modelID, "cline-pass/") || seen[modelID] {
			continue
		}
		cfg.Models = append(cfg.Models, Model{ID: modelID, UpstreamID: modelID})
		seen[modelID] = true
	}
	if e = s.saveConfig(cfg); e != nil {
		return nil, e
	}
	return cfg.Models, nil
}
