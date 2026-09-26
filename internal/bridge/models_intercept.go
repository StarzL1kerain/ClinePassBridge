package bridge

import (
	"encoding/json"
	"strings"
)

// interceptModelsResponse 处理 response.intercept_after：给 CPA 的 /v1/models 响应
// 补上本插件模型的规格。
//
// 为什么需要它：宿主的 OpenAIModels 处理器（sdk/api/handlers/openai/openai_handlers.go）
// 写死了"只保留 4 个必需字段"（id/object/created/owned_by），context_length 与
// max_completion_tokens 在出口被丢掉 —— 所以任何客户端经 CPA 都拿不到模型参数。
// 宿主会把模型列表的响应体交给插件拦截器（见 internal/api/server_models_interceptor_test.go
// 的 TestModelsEndpoint_ExposesResponseToPluginInterceptors_OpenAI），我们在这里补回来。
//
// 只处理形如 {"object":"list","data":[...]} 的响应；其它任何响应（尤其是对话响应）原样返回。
func (s *Service) interceptModelsResponse(raw json.RawMessage) (any, error) {
	var in struct {
		Body            []byte          `json:"Body"`
		ResponseHeaders json.RawMessage `json:"ResponseHeaders"`
		StatusCode      int             `json:"StatusCode"`
	}
	if e := json.Unmarshal(raw, &in); e != nil || len(in.Body) == 0 {
		return modelsInterceptResult(nil), nil
	}
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if json.Unmarshal(in.Body, &list) != nil || list.Object != "list" || len(list.Data) == 0 {
		return modelsInterceptResult(nil), nil
	}
	// 本插件注册过的模型（客户端名与上游名都算）→ 规格
	known := map[string]Model{}
	for _, m := range s.config().Models {
		if id := strings.TrimSpace(m.ID); id != "" {
			known[id] = m
		}
		if upstream := strings.TrimSpace(m.UpstreamID); upstream != "" {
			known[upstream] = m
		}
	}
	changed := false
	for _, entry := range list.Data {
		id, _ := entry["id"].(string)
		m, ok := known[id]
		if !ok {
			continue
		}
		// 只补缺的字段，已有值不动（宿主将来自己带上时不会冲突）。
		if m.ContextLength > 0 && entry["context_length"] == nil {
			entry["context_length"] = m.ContextLength
			changed = true
		}
		if m.MaxCompletionTokens > 0 && entry["max_completion_tokens"] == nil {
			entry["max_completion_tokens"] = m.MaxCompletionTokens
			changed = true
		}
	}
	if !changed {
		return modelsInterceptResult(nil), nil
	}
	body, e := json.Marshal(list)
	if e != nil {
		return modelsInterceptResult(nil), nil
	}
	return modelsInterceptResult(body), nil
}

// modelsInterceptResult 组装拦截响应：body 为 nil 表示"不改动原响应"。
func modelsInterceptResult(body []byte) map[string]any {
	if len(body) == 0 {
		return map[string]any{}
	}
	return map[string]any{"Body": body}
}
