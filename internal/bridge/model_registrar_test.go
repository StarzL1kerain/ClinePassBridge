package bridge

import "testing"

// 模型规格必须由插件申报：CPA 自己的模型目录只收录内置厂商，插件模型不会被自动补全，
// 因此"直接加到 CPA 时规格正确、换成插件后规格丢失"这个差异，只能靠 model_registrar 补上。
func TestModelRegistrationDeclaresSpecsAndUpstreamAlias(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	body := jsonBytes(map[string]any{"models": []any{
		map[string]any{
			"id": "deepseek-v4.1-flash", "upstream_id": "cline-pass/deepseek-v4.1-flash",
			"context_length": 1000000, "max_completion_tokens": 65536,
		},
	}})
	if _, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: body})); err != nil {
		t.Fatalf("保存模型映射失败：%v", err)
	}

	reg, ok := s.modelRegistration().(map[string]any)
	if !ok {
		t.Fatalf("modelRegistration 返回值类型异常：%#v", s.modelRegistration())
	}
	models, ok := reg["Models"].([]map[string]any)
	if !ok || len(models) != 2 {
		t.Fatalf("应同时注册「客户端名」与「上游名」两个条目，实际 %#v", reg["Models"])
	}
	for _, m := range models {
		if m["ContextLength"] != 1000000 || m["MaxCompletionTokens"] != 65536 {
			t.Fatalf("规格字段未申报：%#v", m)
		}
	}
	if models[0]["ID"] != "deepseek-v4.1-flash" {
		t.Fatalf("第一条应当是客户端模型名，实际 %#v", models[0]["ID"])
	}
	if models[1]["ID"] != "cline-pass/deepseek-v4.1-flash" {
		t.Fatalf("第二条应当是上游模型名，实际 %#v", models[1]["ID"])
	}

	// 宿主通过 model.register（model_registrar 能力）拿模型，形状必须与 model.static 一致。
	if _, err := s.Handle("model.register", nil); err != nil {
		t.Fatalf("model.register 未实现：%v", err)
	}
}
