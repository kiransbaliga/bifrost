package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/valkyrie-stack/bifrost/internal/api"
	"github.com/valkyrie-stack/bifrost/internal/builder"
	"github.com/valkyrie-stack/bifrost/internal/config"
	"github.com/valkyrie-stack/bifrost/internal/domain"
	"github.com/valkyrie-stack/bifrost/internal/proxy"
	"github.com/valkyrie-stack/bifrost/internal/runtime"
	"github.com/valkyrie-stack/bifrost/internal/store"
)

func TestAPI_ProjectsAndSystemStatus(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "bifrost-api-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cfg := config.DefaultConfig()
	cfg.Server.DataDir = tempDir

	s, _ := store.NewStore(tempDir)
	defer s.Close()

	rt, _ := runtime.NewContainerClient("/var/run/kiri.sock", false)
	p := proxy.NewProxy(tempDir, "", "", false, nil)
	b := builder.NewBuilder(cfg, s, rt, p)

	apiService := api.NewAPI(cfg, s, b, rt)
	mux := http.NewServeMux()
	apiService.RegisterRoutes(mux)

	// 1. Test /api/v1/system/status
	reqStatus := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	wStatus := httptest.NewRecorder()
	mux.ServeHTTP(wStatus, reqStatus)

	if wStatus.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from system status, got %d", wStatus.Code)
	}

	var metrics domain.SystemMetrics
	if err := json.NewDecoder(wStatus.Body).Decode(&metrics); err != nil {
		t.Fatalf("failed to parse metrics JSON: %v", err)
	}
	if metrics.TotalRAMMB == 0 {
		t.Errorf("expected non-zero total RAM")
	}

	// 2. Create Project via POST /api/v1/projects
	projectPayload := map[string]any{
		"name":       "Demo Project",
		"build_type": "static",
		"domains":    []string{"demo.example.com"},
		"output_dir": "dist",
	}
	body, _ := json.Marshal(projectPayload)
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	wCreate := httptest.NewRecorder()
	mux.ServeHTTP(wCreate, reqCreate)

	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", wCreate.Code, wCreate.Body.String())
	}

	var created domain.Project
	if err := json.NewDecoder(wCreate.Body).Decode(&created); err != nil {
		t.Fatalf("failed to parse created project: %v", err)
	}
	if created.DeployToken == "" || created.WebhookSecret == "" {
		t.Errorf("expected auto-generated DeployToken and WebhookSecret")
	}

	// 3. Test GitHub Webhook HMAC validation
	webhookPayload := []byte(`{
		"ref": "refs/heads/main",
		"after": "abcdef123456",
		"head_commit": {
			"id": "abcdef123456",
			"message": "test: trigger build",
			"author": { "name": "Tester" }
		}
	}`)

	mac := hmac.New(sha256.New, []byte(created.WebhookSecret))
	mac.Write(webhookPayload)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	reqHook := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/github/"+created.ID, bytes.NewReader(webhookPayload))
	reqHook.Header.Set("X-Hub-Signature-256", signature)
	wHook := httptest.NewRecorder()
	mux.ServeHTTP(wHook, reqHook)

	if wHook.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted from valid GitHub webhook, got %d: %s", wHook.Code, wHook.Body.String())
	}
}
