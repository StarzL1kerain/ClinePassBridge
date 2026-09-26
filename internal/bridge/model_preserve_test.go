package bridge

import "testing"

// 前端保存映射时可能只回传 id 与 upstream_id（它拿的是"刷新自动补规格之前"的快照），
// 后端必须保留已有的规格与上游元数据 —— 否则刚自动填好的规格会被一次保存原样覆盖。
func TestSavingModelsPreservesAutoFilledSpecs(t *testing.T) {
	s := registeredService(t, "stream-aggregate")

	// 先按"自动补好的状态"存一次：带规格与上游元数据。
	first := jsonBytes(map[string]any{"models": []any{
		map[string]any{
			"id": "deepseek-v4-pro", "upstream_id": "cline-pass/deepseek-v4-pro",
			"name": "Z.ai: GLM 5.3", "description": "规格说明",
			"tags": []any{"NEW"}, "context_length": 1048576, "max_completion_tokens": 384000,
		},
	}})
	if _, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: first})); err != nil {
		t.Fatalf("第一次保存失败：%v", err)
	}

	// 再模拟前端：只回传 id/upstream_id（不带任何元数据）。
	second := jsonBytes(map[string]any{"models": []any{
		map[string]any{"id": "deepseek-v4-pro", "upstream_id": "cline-pass/deepseek-v4-pro"},
		map[string]any{"id": "deepseek-v4.1-flash", "upstream_id": "cline-pass/deepseek-v4.1-flash"},
	}})
	if _, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: second})); err != nil {
		t.Fatalf("第二次保存失败：%v", err)
	}

	var kept, added *Model
	for i := range s.config().Models {
		switch s.config().Models[i].UpstreamID {
		case "cline-pass/deepseek-v4-pro":
			kept = &s.config().Models[i]
		case "cline-pass/deepseek-v4.1-flash":
			added = &s.config().Models[i]
		}
	}
	if kept == nil || added == nil {
		t.Fatalf("保存后的映射不对：%#v", s.config().Models)
	}
	if kept.ContextLength != 1048576 || kept.MaxCompletionTokens != 384000 {
		t.Fatalf("已有规格被覆盖了：%#v", *kept)
	}
	if kept.Name != "Z.ai: GLM 5.3" || kept.Description != "规格说明" || len(kept.Tags) != 1 {
		t.Fatalf("上游元数据被覆盖了：%#v", *kept)
	}
	// 新加的模型本来就是空的，不该凭空冒出规格
	if added.ContextLength != 0 || added.MaxCompletionTokens != 0 {
		t.Fatalf("新模型不该被填上别人的规格：%#v", *added)
	}
}
