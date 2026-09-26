package bridge

import (
	"testing"
)

// 上游目录里每个模型都带 name/description/tags，刷新时必须原样接住 ——
// description 里就是模型规格（例如 "… with 1M context window"），
// 以前只留 id/upstream_id，CPA 与控制台里就看不到模型最初的参数。
func TestRefreshModelsKeepsUpstreamMetadata(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newOAuthHost(jsonStatusPlan(200, map[string]any{"clinePass": []any{
		map[string]any{
			"id":          "cline-pass/deepseek-v4.1-flash",
			"name":        "Deepseek-v4.1-Flash",
			"description": "Fast and efficient with 1M context window ",
			"tags":        []any{"NEW", "  "},
		},
		map[string]any{"id": "cline-pass/plain"},
	}}))
	s.SetHost(h.call)

	models, err := s.refreshModels("")
	if err != nil {
		t.Fatalf("refreshModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("模型数 = %d，期望 2（%#v）", len(models), models)
	}
	first := models[0]
	if first.ID != "deepseek-v4.1-flash" || first.UpstreamID != "cline-pass/deepseek-v4.1-flash" {
		t.Fatalf("id/upstream_id 被改动：%#v", first)
	}
	if first.Name != "Deepseek-v4.1-Flash" {
		t.Fatalf("name 未接住：%#v", first.Name)
	}
	if first.Description != "Fast and efficient with 1M context window" {
		t.Fatalf("description 未接住（应去掉首尾空白）：%q", first.Description)
	}
	if len(first.Tags) != 1 || first.Tags[0] != "NEW" {
		t.Fatalf("tags 未接住或未过滤空值：%#v", first.Tags)
	}
	// 没有 name/description 的条目仍要能正常返回（老目录/兼容）。
	if second := models[1]; second.Name != "" || second.Description != "" || len(second.Tags) != 0 {
		t.Fatalf("缺字段的条目不该凭空补值：%#v", second)
	}
}
