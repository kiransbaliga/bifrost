package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valkyrie-stack/bifrost/internal/domain"
	"github.com/valkyrie-stack/bifrost/internal/proxy"
)

func TestProxy_StaticDirectServeAndSPAFallback(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "bifrost-proxy-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create test static files (index.html and assets/main.js)
	distDir := filepath.Join(tempDir, "dist")
	_ = os.MkdirAll(filepath.Join(distDir, "assets"), 0755)

	indexPath := filepath.Join(distDir, "index.html")
	_ = os.WriteFile(indexPath, []byte("<!DOCTYPE html><html><body><h1>Root SPA</h1></body></html>"), 0644)

	jsPath := filepath.Join(distDir, "assets", "main.js")
	_ = os.WriteFile(jsPath, []byte("console.log('loaded');"), 0644)

	dashboardHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Dashboard"))
	})

	p := proxy.NewProxy(tempDir, "bifrost.local", "", false, dashboardHandler)

	// Map route to static dist
	routes := []*domain.RouteTarget{
		{
			Domain:     "spa.example.com",
			ProjectID:  "my-spa",
			IsStatic:   true,
			StaticPath: distDir,
		},
	}
	p.UpdateRoutes(routes)

	// 1. Test existing asset: /assets/main.js
	req := httptest.NewRequest(http.MethodGet, "http://spa.example.com/assets/main.js", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for asset, got %d", resp.StatusCode)
	}
	if string(body) != "console.log('loaded');" {
		t.Errorf("unexpected body content: %s", string(body))
	}
	if resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("expected immutable cache header for assets, got: %s", resp.Header.Get("Cache-Control"))
	}

	// 2. Test SPA fallback on virtual client route: /dashboard/settings
	reqSPA := httptest.NewRequest(http.MethodGet, "http://spa.example.com/dashboard/settings", nil)
	wSPA := httptest.NewRecorder()
	p.ServeHTTP(wSPA, reqSPA)

	respSPA := wSPA.Result()
	bodySPA, _ := io.ReadAll(respSPA.Body)
	if respSPA.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for SPA fallback route, got %d", respSPA.StatusCode)
	}
	if string(bodySPA) != "<!DOCTYPE html><html><body><h1>Root SPA</h1></body></html>" {
		t.Errorf("expected root index.html on SPA fallback, got: %s", string(bodySPA))
	}
}

func TestProxy_UnmappedDomainReturns404(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "bifrost-unmapped-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dashboardHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Dashboard"))
	})

	p := proxy.NewProxy(tempDir, "bifrost.local", "", false, dashboardHandler)

	req := httptest.NewRequest(http.MethodGet, "http://unknown.randomdomain.com/path", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unmapped external domain, got %d", w.Result().StatusCode)
	}
}
