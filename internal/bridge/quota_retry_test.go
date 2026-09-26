package bridge

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 传输层抖动（EOF、连接重置）不该让整个额度刷新失败：重试后成功即返回正常额度。
func TestQuotaRetriesTransientTransportFailures(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-01M2W9Y7XBSS4X00YMH9NARQ4E")
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	transient := errors.New(`execute host http request: Get "https://api.cline.bot/api/v1/users/me/plan": EOF`)
	h := newOAuthHost(
		hostPlan{openErr: transient},
		hostPlan{openErr: transient},
		planResponse(),
		usageLimitsResponse(),
		balanceResponse(-32221),
	)
	s.SetHost(h.call)

	result, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	}))
	if err != nil {
		t.Fatalf("连续两次传输失败后，重试应当成功，实际：%v", err)
	}
	monthly := bucketByWindow(t, result, "monthly")
	if math.Abs(monthly["remainingFraction"].(float64)-0.5) > 1e-9 {
		t.Fatalf("monthly remainingFraction = %v, want 0.5", monthly["remainingFraction"])
	}
}

// 重试用尽后仍失败，才把传输错误暴露出来（且带原始原因）。
func TestQuotaSurfacesTransportFailureAfterRetries(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-01M2W9Y7XBSS4X00YMH9NARQ4E")
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	transient := errors.New("EOF")
	h := newOAuthHost(
		hostPlan{openErr: transient},
		hostPlan{openErr: transient},
		hostPlan{openErr: transient},
	)
	s.SetHost(h.call)

	_, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	}))
	if err == nil {
		t.Fatal("三次传输都失败时应当报错")
	}
	if !strings.Contains(err.Error(), "无法连接 Cline 服务") {
		t.Fatalf("错误信息应带可读前缀，实际：%v", err)
	}
	// /users/me/plan 试满次数后按 v0.1.9 的规则降级，随后必需的 usage-limits 再各自重试，
	// 所以这里按 URL 精确断言"第一个接口确实试满了"，而不是数总调用次数。
	planCalls := 0
	for _, call := range h.calls {
		if strings.HasSuffix(call.url, "/users/me/plan") {
			planCalls++
		}
	}
	if planCalls != clineRequestAttempts {
		t.Fatalf("/users/me/plan 应尝试 %d 次，实际 %d 次（全部调用 %d 次）", clineRequestAttempts, planCalls, len(h.calls))
	}
}

// 限流与 5xx 也值得重试（与 CommandCodeBridge 同一套规则），4xx 则立刻返回。
func TestQuotaRetriesRateLimitAndServerErrors(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	credential := oauthCredential("usr-01M2W9Y7XBSS4X00YMH9NARQ4E")
	if _, err := s.Handle("auth.parse", jsonBytes(map[string]any{
		"Provider": Provider, "FileName": credential.ID + ".json", "RawJSON": jsonBytes(credential),
	})); err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	h := newOAuthHost(
		jsonStatusPlan(429, map[string]any{"error": "slow down"}),
		jsonStatusPlan(503, map[string]any{"error": "upstream busy"}),
		planResponse(),
		usageLimitsResponse(),
		balanceResponse(-32221),
	)
	s.SetHost(h.call)

	result, err := s.Handle("quota.fetch", jsonBytes(map[string]any{
		"auth_id": credential.ID, "provider": Provider, "storage_json": jsonBytes(credential),
	}))
	if err != nil {
		t.Fatalf("429/503 后重试应当成功，实际：%v", err)
	}
	monthly := bucketByWindow(t, result, "monthly")
	if math.Abs(monthly["remainingFraction"].(float64)-0.5) > 1e-9 {
		t.Fatalf("monthly remainingFraction = %v, want 0.5", monthly["remainingFraction"])
	}
}
