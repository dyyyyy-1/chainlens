package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newAuthedRouter(token string) http.Handler {
	return NewRouter(Deps{
		Events:  &fakeEvents{coverage: testCoverage()},
		Profile: &fakeProfile{},
		// 关掉限流，免得多个用例共用同一个来源 IP 时互相影响。
		RateLimitPerMinute: 0,
		APIToken:           token,
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
	handler := newAuthedRouter("")

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
