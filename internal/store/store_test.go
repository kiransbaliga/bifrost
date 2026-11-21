package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valkyrie-stack/bifrost/internal/domain"
	"github.com/valkyrie-stack/bifrost/internal/store"
)

func TestStore_ProjectAndRouteLifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "bifrost-store-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	s, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to initialize store: %v", err)
	}
	defer s.Close()

	// Verify DB file was created
	if _, err := os.Stat(filepath.Join(tempDir, "bifrost.db")); err != nil {
		t.Fatalf("expected bifrost.db to exist: %v", err)
	}

	// 1. Create Project
	p := &domain.Project{
		ID:            "test-app",
		Name:          "Test App",
		RepoURL:       "https://github.com/example/test-app",
		Branch:        "main",
		BuildType:     domain.BuildTypeStatic,
		OutputDir:     "dist",
		Port:          3000,
		MemoryLimitMB: 256,
		Domains:       []string{"test.example.com"},
		EnvVars:       map[string]string{"NODE_ENV": "production"},
		WebhookSecret: "secret123",
		DeployToken:   "tok_123",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}

	if err := s.SaveProject(p); err != nil {
		t.Fatalf("failed to save project: %v", err)
	}

	// 2. Retrieve Project
	retrieved, err := s.GetProject("test-app")
	if err != nil {
		t.Fatalf("failed to get project: %v", err)
	}
	if retrieved.Name != "Test App" || retrieved.BuildType != domain.BuildTypeStatic {
		t.Errorf("mismatched project data: %+v", retrieved)
	}
	if len(retrieved.Domains) != 1 || retrieved.Domains[0] != "test.example.com" {
		t.Errorf("expected 1 domain, got: %v", retrieved.Domains)
	}

	// 3. Save Deployment
	dep := &domain.Deployment{
		ID:         "dep-001",
		ProjectID:  p.ID,
		CommitHash: "abcdef123456",
		CommitMsg:  "feat: initial commit",
		Author:     "Developer",
		Status:     domain.StatusHealthy,
		Trigger:    domain.TriggerArtifactPush,
		StartedAt:  time.Now().UTC(),
	}
	if err := s.SaveDeployment(dep); err != nil {
		t.Fatalf("failed to save deployment: %v", err)
	}

	deps, err := s.ListDeployments(p.ID, 10)
	if err != nil || len(deps) != 1 {
		t.Fatalf("failed to list deployments: %v (count: %d)", err, len(deps))
	}

	// 4. Routes
	route := &domain.RouteTarget{
		Domain:     "test.example.com",
		ProjectID:  p.ID,
		IsStatic:   true,
		StaticPath: "/var/lib/bifrost/projects/test-app/current/dist",
	}
	if err := s.SaveRoute(route); err != nil {
		t.Fatalf("failed to save route: %v", err)
	}

	routes, err := s.ListRoutes()
	if err != nil || len(routes) != 1 {
		t.Fatalf("failed to list routes: %v (count: %d)", err, len(routes))
	}
	if routes[0].Domain != "test.example.com" || !routes[0].IsStatic {
		t.Errorf("unexpected route target: %+v", routes[0])
	}

	// 5. Delete Project cascades
	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatalf("failed to delete project: %v", err)
	}

	if _, err := s.GetProject(p.ID); err == nil {
		t.Errorf("expected project to be deleted")
	}
}
