package bridge

import (
	"encoding/json"
	"testing"
)

// CPA 的 /v1/models 只输出 id/object/created/owned_by（宿主写死的过滤），
// 插件通过 response.intercept_after 把规格补回来 —— 这样客户端经 CPA 也能看到参数。
func TestModelsInterceptEnrichesOnlyOwnModels(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	body := jsonBytes(map[string]any{"models": []any{
		map[string]any{
			"id": "deepseek-v4-pro", "upstream_id": "cline-pass/deepseek-v4-pro",
			"context_length": 1048576, "max_completion_tokens": 384000,
		},
	}})
	if _, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: body})); err != nil {
		t.Fatalf("准备映射失败：%v", err)
	}

	modelsList := jsonBytes(map[string]any{
		"object": "list",
		"data": []any{
			map[string]any{"id": "deepseek-v4-pro", "object": "model", "owned_by": "cline-pass"},
			map[string]any{"id": "cline-pass/deepseek-v4-pro", "object": "model", "owned_by": "cline-pass"},
			map[string]any{"id": "someone-elses-model", "object": "model", "owned_by": "other"},
		},
	})
	result, err := s.Handle("response.intercept_after", jsonBytes(map[string]any{"Body": modelsList, "StatusCode": 200}))
	if err != nil {
		t.Fatalf("拦截失败：%v", err)
	}
	out, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("拦截返回类型异常：%#v", result)
	}
	changed, ok := out["Body"].([]byte)
	if !ok || len(changed) == 0 {
		t.Fatalf("应当改写响应体，实际：%#v", out)
	}
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(changed, &list); err != nil {
		t.Fatalf("改写后的响应不是合法 JSON：%v", err)
	}
	if list.Object != "list" || len(list.Data) != 3 {
		t.Fatalf("响应结构被破坏：%#v", list)
	}
	for i := 0; i < 2; i++ {
		if got := list.Data[i]["context_length"]; got != float64(1048576) {
			t.Fatalf("第 %d 条没补上 context_length：%#v", i, list.Data[i])
		}
		if got := list.Data[i]["max_completion_tokens"]; got != float64(384000) {
			t.Fatalf("第 %d 条没补上 max_completion_tokens：%#v", i, list.Data[i])
		}
	}
	if _, has := list.Data[2]["context_length"]; has {
		t.Fatalf("不该动别人的模型：%#v", list.Data[2])
	}

	// 对话响应（不是模型列表）必须原样返回，绝不改写
	chat := jsonBytes(map[string]any{"id": "x", "object": "chat.completion", "choices": []any{}})
	result, err = s.Handle("response.intercept_after", jsonBytes(map[string]any{"Body": chat, "StatusCode": 200}))
	if err != nil {
		t.Fatalf("拦截失败：%v", err)
	}
	if out, ok := result.(map[string]any); !ok || len(out) != 0 {
		t.Fatalf("非模型列表响应不该被改写：%#v", result)
	}

	// 已经带规格的条目不应重复改写
	already := jsonBytes(map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "deepseek-v4-pro", "object": "model", "context_length": 1, "max_completion_tokens": 2},
	}})
	result, err = s.Handle("response.intercept_after", jsonBytes(map[string]any{"Body": already, "StatusCode": 200}))
	if err != nil {
		t.Fatalf("拦截失败：%v", err)
	}
	if out, ok := result.(map[string]any); !ok || len(out) != 0 {
		t.Fatalf("已有规格时不该改写：%#v", result)
	}
}
