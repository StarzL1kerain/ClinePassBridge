package bridge

import "testing"

// 上游两个目录命名不同：Pass 商品目录是 cline-pass/deepseek-v4.1-flash，
// 完整目录是 deepseek/deepseek-v4.1-flash —— 规格要按"名字最后一段"对齐后自动填上，
// 且只填空值（用户填过的不覆盖），已保存的映射也要顺手补好，用户不必手填。
func TestRefreshModelsAutoFillsSpecsFromCatalog(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newOAuthHost(
		jsonStatusPlan(200, map[string]any{"clinePass": []any{
			map[string]any{"id": "cline-pass/deepseek-v4.1-flash", "description": "with 1M context window"},
		}}),
		jsonStatusPlan(200, map[string]any{"data": []any{
			map[string]any{
				"id": "deepseek/deepseek-v4.1-flash", "context_length": 1048576,
				"top_provider": map[string]any{"max_completion_tokens": 943718},
			},
		}}),
	)
	s.SetHost(h.call)

	models, err := s.refreshModels("")
	if err != nil {
		t.Fatalf("refreshModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("候选数 = %d：%#v", len(models), models)
	}
	if models[0].ContextLength != 1048576 || models[0].MaxCompletionTokens != 943718 {
		t.Fatalf("候选的规格未自动填充：%#v", models[0])
	}

	// 已保存映射里空着的规格同样被补上（默认映射就是 cline-pass/deepseek-v4.1-flash）。
	saved := s.config().Models
	if len(saved) == 0 {
		t.Fatal("默认映射不应为空")
	}
	for _, m := range saved {
		if m.UpstreamID == "cline-pass/deepseek-v4.1-flash" {
			if m.ContextLength != 1048576 || m.MaxCompletionTokens != 943718 {
				t.Fatalf("已保存映射的规格未补上：%#v", m)
			}
			return
		}
	}
	t.Fatalf("没找到默认映射：%#v", saved)
}
