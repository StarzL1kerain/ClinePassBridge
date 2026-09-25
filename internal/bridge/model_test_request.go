package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// modelTestDiagnostics 只在显式触发管理端「模型测试」时收集上游原始响应；
// 正常流量不会挂上它，且占用受 max_response_bytes 上限约束。
type modelTestDiagnostics struct {
	body      bytes.Buffer
	limit     int
	truncated bool
}

func (d *modelTestDiagnostics) capture(b []byte) {
	if d == nil {
		return
	}
	remaining := d.limit - d.body.Len()
	if len(b) > remaining {
		b = b[:remaining]
		d.truncated = true
	}
	d.body.Write(b)
}

func (d *modelTestDiagnostics) transportError(message string) {
	if d != nil {
		d.capture([]byte("\n传输错误：" + message))
	}
}

// redactModelTest 把诊断文本里的凭据替换掉，避免测试结果把 key 或令牌带出去。
func redactModelTest(text string, c Credential) string {
	for _, secret := range []string{c.APIKey, c.bearerToken(), c.RefreshToken} {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[已脱敏]")
		encoded, _ := json.Marshal(secret)
		if len(encoded) > 2 {
			text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[已脱敏]")
		}
	}
	return secretPattern.ReplaceAllString(text, "[已脱敏]")
}

// testModel 处理 POST /models/test：用指定凭据对指定模型发一条最短的流式请求，
// 把结果与上游诊断返回给管理端，便于在不接客户端的情况下定位问题。
func (s *Service) testModel(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	defer s.active.Done()
	var in struct {
		Model        string `json:"model"`
		UpstreamID   string `json:"upstream_id"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.Model == "" || in.CredentialID == "" {
		return managementJSON(400, map[string]any{"error": "请选择模型与测试凭据"})
	}
	upstream, err := s.resolveModel(in.Model)
	if err != nil {
		return managementJSON(400, map[string]any{"error": safeError(err)})
	}
	if upstream != in.UpstreamID {
		return managementJSON(409, map[string]any{"error": "模型映射已变更，请刷新后重试"})
	}
	// 同一个「模型 + 凭据」同时只允许一次测试，整体最多 3 个。
	key := string(jsonBytes([]string{in.Model, in.CredentialID}))
	s.mu.Lock()
	if s.modelTests[key] || len(s.modelTests) >= 3 {
		s.mu.Unlock()
		return managementJSON(429, map[string]any{"error": "已有模型正在测试，请稍后重试（最多同时 3 个）"})
	}
	if s.modelTests == nil {
		s.modelTests = map[string]bool{}
	}
	s.modelTests[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.modelTests, key); s.mu.Unlock() }()

	cfg := s.config()
	start := time.Now()
	req := ExecutorRequest{
		AuthID: in.CredentialID, Model: in.Model, HostCallbackID: r.HostCallbackID,
		deadline: start.Add(time.Duration(cfg.TimeoutSeconds) * time.Second),
		Payload:  []byte(`{"messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":64}`),
	}
	j, credential, upstream, err := s.prepare(req)
	if err == nil && upstream != in.UpstreamID {
		err = fail(409, "模型映射已变更，请刷新后重试")
	}
	entry := s.newLog(req, credential, upstream)
	attempt := Attempt{Mode: "model-test", Provider: "unknown", ProviderSource: "not_reported"}
	diagnostics := &modelTestDiagnostics{limit: cfg.MaxResponseBytes}
	// CPA 的管理接口只支持全局代理，无法为单个凭据指定代理。
	if err == nil && strings.TrimSpace(credential.ProxyURL) != "" {
		err = fail(400, "此 CPA 管理接口暂不支持凭据独立代理的模型测试，请选择使用全局代理的凭据")
	}
	if err == nil {
		var stream upstreamStream
		stream, err = s.request(req, credential, j, true, diagnostics)
		if err == nil {
			var completion *completion
			completion, err = s.consumeSSE(stream, in.Model, &entry, &attempt, start, nil)
			if err == nil {
				_, err = completion.result(in.Model)
			}
		}
	}
	duration := time.Since(start).Milliseconds()
	detail := ""
	if err != nil {
		detail = err.Error()
		if diagnostics.body.Len() > 0 {
			detail += "\n\n上游响应 / 传输详情：\n" + diagnostics.body.String()
		}
		if diagnostics.truncated {
			detail += fmt.Sprintf("\n\n[详情超出配置的 %d 字节响应上限，已截断]", cfg.MaxResponseBytes)
		}
		if statusOf(err) == 504 {
			detail += fmt.Sprintf("\n请求超时设置：%d 秒", cfg.TimeoutSeconds)
		}
		detail = redactModelTest(detail, credential)
	}
	attempt.Status, attempt.DurationMS = statusOf(err), duration
	if err != nil {
		attempt.Error = safeError(fmt.Errorf("%s", redactModelTest(err.Error(), credential)))
	}
	entry.Status, entry.DurationMS, entry.Error = attempt.Status, duration, redactModelTest(attempt.Error, credential)
	entry.Attempts = []Attempt{attempt}
	s.appendLog(entry)
	// 上游 401/403 是测试结果，不是 CPA 管理鉴权失败。
	return managementJSON(200, map[string]any{
		"ok": err == nil, "status": statusOf(err), "error": detail, "duration_ms": duration,
		"ttft_ms": entry.TTFTMS, "model": in.Model, "upstream_id": upstream,
		"credential_id": credential.ID, "credential_label": credential.Label,
		"provider": entry.Provider, "tested_at": time.Now().UTC(),
	})
}
