package bridge

import "testing"

// 模型规格必须由插件申报：CPA 自己的模型目录只收录内置厂商，插件模型不会被自动补全。
// 同时只注册「客户端名」一个条目 —— 上游名是内部转发用的，不该出现在客户端的模型列表里
// （用户给模型改名就是为了区分渠道，多注册旧名字会把列表弄乱）。
func TestModelRegistrationDeclaresSpecsOnly(t *testing.T) {
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
	if !ok || len(models) != 1 {
		t.Fatalf("只应注册客户端名一个条目，实际 %#v", reg["Models"])
	}
	for _, m := range models {
		if m["ContextLength"] != 1000000 || m["MaxCompletionTokens"] != 65536 {
			t.Fatalf("规格字段未申报：%#v", m)
		}
	}
	if models[0]["ID"] != "deepseek-v4.1-flash" {
		t.Fatalf("应当是客户端模型名，实际 %#v", models[0]["ID"])
	}

	// 宿主通过 model.register（model_registrar 能力）拿模型，形状必须与 model.static 一致。
	if _, err := s.Handle("model.register", nil); err != nil {
		t.Fatalf("model.register 未实现：%v", err)
	}
}
