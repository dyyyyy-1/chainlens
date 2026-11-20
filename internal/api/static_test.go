package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStaticRouter 造一个托管临时目录的路由，用来验证 SPA 回退与 /api 的隔离。
func newStaticRouter(t *testing.T) http.Handler {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>INDEX"), 0o600); err != nil {
		t.Fatalf("写入 index.html 失败：%v", err)
	}

	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o750); err != nil {
		t.Fatalf("创建 assets 目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(assets, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("写入静态资源失败：%v", err)
	}

	return NewRouter(Deps{Events: &fakeEvents{}, Profile: &fakeProfile{}, WebDir: dir})
}

func TestStaticServesIndexAtRoot(t *testing.T) {
	recorder := get(t, newStaticRouter(t), "/")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "INDEX") {
		t.Fatalf("根路径未返回 index.html：%s", recorder.Body.String())
	}
}

func TestStaticServesAsset(t *testing.T) {
	recorder := get(t, newStaticRouter(t), "/assets/app.js")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "console.log") {
		t.Fatalf("静态资源内容不对：%s", recorder.Body.String())
	}
}

func TestStaticFallsBackToIndexForClientRoutes(t *testing.T) {
	recorder := get(t, newStaticRouter(t), "/address/0xabc")
	if recorder.Code != http.StatusOK {
		t.Fatalf("前端路由回退失败，状态码 = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "INDEX") {
		t.Fatalf("回退内容不是 index.html：%s", recorder.Body.String())
	}
}

// TestStaticDoesNotSwallowUnknownAPI 是最重要的一条：
// 未知的 /api 路径若回退到 index.html，客户端会拿到 200 + HTML，
// 把「接口不存在」误读成「接口返回了奇怪的东西」。
func TestStaticDoesNotSwallowUnknownAPI(t *testing.T) {
	recorder := get(t, newStaticRouter(t), "/api/does-not-exist")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, want 404", recorder.Code)
	}
	if code := decodeBody[errorResponseDTO](t, recorder).Error.Code; code != "not_found" {
		t.Fatalf("错误码 = %q, want not_found", code)
	}
}

func TestStaticTraversalCannotEscapeRoot(t *testing.T) {
	// ServeMux 会对含 ".." 的路径做清理并回 307；清理后要么落在托管目录内，
	// 要么走 index.html 回退。判定标准只有一条：**没有读到目录外的文件**。
	recorder := get(t, newStaticRouter(t), "/../../etc/passwd")

	if recorder.Code != http.StatusOK && recorder.Code != http.StatusMovedPermanently && recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("状态码 = %d，属于未预期分支", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "root:") {
		t.Fatalf("越权读取了托管目录之外的文件：%s", recorder.Body.String())
	}
}

func TestNoStaticWhenWebDirEmpty(t *testing.T) {
	handler := NewRouter(Deps{Events: &fakeEvents{}, Profile: &fakeProfile{}})

	// 未配置 WebDir 时根路径不存在，应回 404 而不是 index.html。
	if code := get(t, handler, "/").Code; code != http.StatusNotFound {
		t.Fatalf("未配置 WebDir 时根路径状态码 = %d, want 404", code)
	}
}
