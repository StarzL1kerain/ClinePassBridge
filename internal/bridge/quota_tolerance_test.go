package bridge

import (
	"math"
	"testing"
)

// /users/me/plan 只提供套餐名、窗口上限与当前周期。用 API key 认证时它可能不开放，
// 此时必须降级展示额度窗口，而不是让整个额度查询失败（否则面板只显示"获取失败"）。
func TestQuotaFetchKeepsWindowsWhenPlanEndpointIsUnavailable(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-01M2W9Y7XBSS4X00YMH9NARQ4E")
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(
		jsonStatusPlan(404, map[string]any{"error": "not found"}),
		usageLimitsResponse(),
		balanceResponse(-32221),
	)
	s.SetHost(h.call)

	result, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	}))
	if err != nil {
		t.Fatalf("套餐接口不可用时额度查询不应失败：%v", err)
	}

	for window, want := range map[string]float64{"five_hour": 1, "seven_day": 0, "monthly": 0.5} {
		bucket := bucketByWindow(t, result, window)
		if math.Abs(bucket["remainingFraction"].(float64)-want) > 1e-9 {
			t.Fatalf("%s remainingFraction = %v, want %v", window, bucket["remainingFraction"], want)
		}
	}

	response, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("额度响应类型 = %T", result)
	}
	if _, ok := response["subscription"]; ok {
		t.Fatalf("套餐信息缺失时不应报告 subscription：%#v", response["subscription"])
	}
	if summary, ok := response["summary"].([]any); ok {
		for _, item := range summary {
			metric, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if metric["key"] == "cline_pass_period_days_left" {
				t.Fatalf("套餐信息缺失时不应出现周期剩余天数：%#v", metric)
			}
		}
	}
}
