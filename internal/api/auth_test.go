package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAuthedRouter(tokens ...string) http.Handler {
	return NewRouter(Deps{
		Events:  &fakeEvents{coverage: testCoverage()},
		Profile: &fakeProfile{},
		// 关掉限流，免得多个用例共用同一个来源 IP 时互相影响。
		RateLimitPerMinute: 0,
		APITokens:          tokens,
	})
}

func getWithAuth(handler http.Handler, target, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestAuthDisabledWhenTokenEmpty(t *testing.T) {
	handler := newAuthedRouter()

	if code := getWithAuth(handler, "/api/stats", "").Code; code != http.StatusOK {
		t.Fatalf("未配置 token 时不应要求鉴权，状态码 = %d", code)
	}
}

func TestAuthRejectsMissingToken(t *testing.T) {
	handler := newAuthedRouter("s3cret")

	recorder := getWithAuth(handler, "/api/stats", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, want 401", recorder.Code)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("401 必须带 WWW-Authenticate")
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "unauthorized" {
		t.Fatalf("错误码 = %q, want unauthorized", code)
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	handler := newAuthedRouter("s3cret")

	cases := []string{
		"Bearer wrong",
		"Bearer s3cret ",   // 尾随空格会被 TrimSpace 去掉，仍应通过——见下一个用例
		"Bearer s3cret-xx", // 前缀正确但值不同
		"Basic s3cret",     // 方案不对
		"s3cret",           // 缺少方案
	}
	for _, header := range cases {
		code := getWithAuth(handler, "/api/stats", header).Code
		want := http.StatusUnauthorized
		if header == "Bearer s3cret " {
			want = http.StatusOK
		}
		if code != want {
			t.Fatalf("Authorization=%q 状态码 = %d, want %d", header, code, want)
		}
	}
}

func TestAuthAcceptsValidToken(t *testing.T) {
	handler := newAuthedRouter("s3cret")

	if code := getWithAuth(handler, "/api/stats", "Bearer s3cret").Code; code != http.StatusOK {
		t.Fatalf("正确 token 状态码 = %d, want 200", code)
	}
	// 方案名大小写不敏感（RFC 7235）。
	if code := getWithAuth(handler, "/api/stats", "bearer s3cret").Code; code != http.StatusOK {
		t.Fatalf("小写方案名状态码 = %d, want 200", code)
	}
}

func TestAuthAcceptsAnyConfiguredToken(t *testing.T) {
	// 轮换场景：新旧口令并存，两者都应通过。
	handler := newAuthedRouter("old-token", "new-token")

	for _, token := range []string{"old-token", "new-token"} {
		if code := getWithAuth(handler, "/api/stats", "Bearer "+token).Code; code != http.StatusOK {
			t.Fatalf("token %q 状态码 = %d, want 200", token, code)
		}
	}

	if code := getWithAuth(handler, "/api/stats", "Bearer other-token").Code; code != http.StatusUnauthorized {
		t.Fatalf("未配置的 token 状态码 = %d, want 401", code)
	}
}

func TestTokenFingerprintDoesNotLeakSecret(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	request.Header.Set("Authorization", "Bearer s3cret")

	fingerprint := tokenFingerprint(request)
	if fingerprint == "" {
		t.Fatal("有 Authorization 头时指纹不应为空")
	}
	if strings.Contains(fingerprint, "s3cret") {
		t.Fatalf("指纹泄漏了明文口令：%q", fingerprint)
	}
	if len(fingerprint) != 8 {
		t.Fatalf("指纹长度 = %d, want 8（SHA-256 前 4 字节）", len(fingerprint))
	}

	// 同一口令必须稳定，不同口令必须不同——否则日志无法做归因。
	if again := tokenFingerprint(request); again != fingerprint {
		t.Fatalf("同一口令的指纹不稳定：%q vs %q", fingerprint, again)
	}
	other := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	other.Header.Set("Authorization", "Bearer other")
	if tokenFingerprint(other) == fingerprint {
		t.Fatal("不同口令得到了相同指纹")
	}

	anonymous := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	if got := tokenFingerprint(anonymous); got != "" {
		t.Fatalf("无 Authorization 头时指纹应为空，得到 %q", got)
	}
}

func TestAuthProtectsEveryEndpoint(t *testing.T) {
	handler := newAuthedRouter("s3cret")

	paths := []string{
		"/api/stats",
		"/api/events",
		"/api/address/" + sampleAddress + "/balances",
		"/api/address/" + sampleAddress + "/approvals",
		"/api/address/" + sampleAddress + "/interactions",
	}
	for _, path := range paths {
		if code := getWithAuth(handler, path, "").Code; code != http.StatusUnauthorized {
			t.Fatalf("%s 未受保护，状态码 = %d", path, code)
		}
	}
}
