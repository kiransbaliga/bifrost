package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/valkyrie-stack/bifrost/internal/builder"
	"github.com/valkyrie-stack/bifrost/internal/config"
	"github.com/valkyrie-stack/bifrost/internal/domain"
	"github.com/valkyrie-stack/bifrost/internal/runtime"
	"github.com/valkyrie-stack/bifrost/internal/store"
	"github.com/valkyrie-stack/bifrost/internal/sysguard"
)

// API holds handlers for the REST API, webhooks, and SSE logs.
type API struct {
	cfg      *config.Config
	store    *store.Store
	builder  *builder.Builder
	rtClient *runtime.ContainerClient
}

// NewAPI initializes the API service.
func NewAPI(cfg *config.Config, s *store.Store, b *builder.Builder, rt *runtime.ContainerClient) *API {
	return &API{
		cfg:      cfg,
		store:    s,
		builder:  b,
		rtClient: rt,
	}
}

// RegisterRoutes registers all API endpoints on the provided mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/system/status", a.handleSystemStatus)
	mux.HandleFunc("/api/v1/projects", a.handleProjects)
	mux.HandleFunc("/api/v1/projects/", a.handleProjectItem)
	mux.HandleFunc("/api/v1/webhook/github/", a.handleGitHubWebhook)
	mux.HandleFunc("/api/v1/deployments/", a.handleDeploymentLogs)
}

func (a *API) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	activeContainers, _ := a.rtClient.CountRunningContainers(r.Context())
	routes, _ := a.store.ListRoutes()
	metrics := sysguard.GetSystemMetrics(activeContainers, len(routes))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metrics)
}

func (a *API) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		projects, err := a.store.ListProjects()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(projects)

	case http.MethodPost:
		var p domain.Project
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		if p.Name == "" {
			http.Error(w, "Project name is required", http.StatusBadRequest)
			return
		}
		if p.ID == "" {
			p.ID = strings.ToLower(strings.ReplaceAll(p.Name, " ", "-")) + "-" + builder.GenerateID()[:4]
		}
		if p.Branch == "" {
			p.Branch = "main"
		}
		if p.BuildType == "" {
			p.BuildType = domain.BuildTypeNode
		}
		if p.Port <= 0 && p.BuildType != domain.BuildTypeStatic {
			p.Port = 3000
		}
		if p.MemoryLimitMB <= 0 {
			p.MemoryLimitMB = a.cfg.Build.DefaultMemoryLimitMB
		}
		if p.WebhookSecret == "" {
			p.WebhookSecret = builder.GenerateID() + builder.GenerateID()
		}
		if p.DeployToken == "" {
			p.DeployToken = "bf_" + builder.GenerateID() + builder.GenerateID()
		}

		if err := a.store.SaveProject(&p); err != nil {
			http.Error(w, "Save project failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) handleProjectItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	parts := strings.Split(path, "/")
	projectID := parts[0]

	if projectID == "" {
		http.NotFound(w, r)
		return
	}

	project, err := a.store.GetProject(projectID)
	if err != nil {
		http.Error(w, "Project not found", http.StatusNotFound)
		return
	}

	// Sub-routes: /deploy or /deploy/artifact or /deployments
	if len(parts) >= 2 {
		sub := parts[1]
		switch sub {
		case "deploy":
			if len(parts) >= 3 && parts[2] == "artifact" {
				a.handleDeployArtifact(w, r, project)
				return
			}
			a.handleManualDeploy(w, r, project)
			return
		case "deployments":
			deps, err := a.store.ListDeployments(project.ID, 20)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(deps)
			return
		}
	}

	// Base project actions
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(project)

	case http.MethodPut:
		var updated domain.Project
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		updated.ID = project.ID
		updated.WebhookSecret = project.WebhookSecret
		updated.DeployToken = project.DeployToken
		updated.ActiveDeploymentID = project.ActiveDeploymentID
		updated.ActiveContainerID = project.ActiveContainerID

		if err := a.store.SaveProject(&updated); err != nil {
			http.Error(w, "Update failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(updated)

	case http.MethodDelete:
		// Stop running container if any
		if project.ActiveContainerID != "" {
			_ = a.rtClient.StopContainer(r.Context(), project.ActiveContainerID, 5)
			_ = a.rtClient.RemoveContainer(r.Context(), project.ActiveContainerID)
		}
		_ = a.store.DeleteRoutesByProject(project.ID)
		_ = a.store.DeleteProject(project.ID)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDeployArtifact receives pre-built tarball from GitHub Actions (ADR-004).
func (a *API) handleDeployArtifact(w http.ResponseWriter, r *http.Request, p *domain.Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify Bearer token
	authHeader := r.Header.Get("Authorization")
	expected := "Bearer " + p.DeployToken
	if authHeader != expected {
		http.Error(w, "Unauthorized: Invalid deploy token", http.StatusUnauthorized)
		return
	}

	dep := &domain.Deployment{
		ID:         builder.GenerateID(),
		ProjectID:  p.ID,
		CommitHash: r.Header.Get("X-Commit-Hash"),
		CommitMsg:  r.Header.Get("X-Commit-Message"),
		Author:     r.Header.Get("X-Commit-Author"),
		Status:     domain.StatusQueued,
		Trigger:    domain.TriggerArtifactPush,
		StartedAt:  time.Now().UTC(),
	}

	if err := a.store.SaveDeployment(dep); err != nil {
		http.Error(w, "Failed to create deployment record: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Stream and unpack
	go func() {
		_ = a.builder.DeployArtifact(r.Context(), p, r.Body, dep)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"deployment_id": dep.ID,
		"status":        "accepted",
		"message":       "Artifact deployment started. Monitor logs via SSE.",
	})
}

func (a *API) handleManualDeploy(w http.ResponseWriter, r *http.Request, p *domain.Project) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dep := &domain.Deployment{
		ID:        builder.GenerateID(),
		ProjectID: p.ID,
		Status:    domain.StatusQueued,
		Trigger:   domain.TriggerManual,
		StartedAt: time.Now().UTC(),
	}

	if err := a.store.SaveDeployment(dep); err != nil {
		http.Error(w, "Failed to initialize deployment: "+err.Error(), http.StatusInternalServerError)
		return
	}

	a.builder.QueueLocalBuild(r.Context(), p, dep)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"deployment_id": dep.ID,
		"status":        "queued",
	})
}

// handleGitHubWebhook handles push webhooks with HMAC-SHA256 validation.
func (a *API) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	projectID := strings.TrimPrefix(r.URL.Path, "/api/v1/webhook/github/")
	project, err := a.store.GetProject(projectID)
	if err != nil {
		http.Error(w, "Project not found", http.StatusNotFound)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	// Verify HMAC signature
	if project.WebhookSecret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifyGitHubHMAC(payload, project.WebhookSecret, sig) {
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	var event struct {
		Ref        string `json:"ref"` // e.g. "refs/heads/main"
		After      string `json:"after"`
		HeadCommit struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
	}

	if err := json.Unmarshal(payload, &event); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	// Verify target branch
	expectedRef := "refs/heads/" + project.Branch
	if event.Ref != expectedRef {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Ignored push to %s (configured branch is %s)", event.Ref, project.Branch)
		return
	}

	dep := &domain.Deployment{
		ID:         builder.GenerateID(),
		ProjectID:  project.ID,
		CommitHash: event.HeadCommit.ID,
		CommitMsg:  event.HeadCommit.Message,
		Author:     event.HeadCommit.Author.Name,
		Status:     domain.StatusQueued,
		Trigger:    domain.TriggerGitHubWebhook,
		StartedAt:  time.Now().UTC(),
	}

	if err := a.store.SaveDeployment(dep); err != nil {
		http.Error(w, "Failed to record deployment: "+err.Error(), http.StatusInternalServerError)
		return
	}

	a.builder.QueueLocalBuild(r.Context(), project, dep)

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "Deployment %s queued for branch %s", dep.ID, project.Branch)
}

// handleDeploymentLogs streams logs using Server-Sent Events (SSE).
func (a *API) handleDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/deployments/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "logs" {
		http.NotFound(w, r)
		return
	}

	depID := parts[0]
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	sentCount := 0
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			logs := a.builder.GetLogs(depID)
			for i := sentCount; i < len(logs); i++ {
				fmt.Fprintf(w, "data: %s\n\n", logs[i])
			}
			if len(logs) > sentCount {
				sentCount = len(logs)
				flusher.Flush()
			}

			// Check if deployment has finished
			dep, err := a.store.GetDeployment(depID)
			if err == nil && dep.FinishedAt != nil {
				fmt.Fprintf(w, "event: done\ndata: %s\n\n", dep.Status)
				flusher.Flush()
				return
			}
		}
	}
}

func verifyGitHubHMAC(payload []byte, secret, signatureHeader string) bool {
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return false
	}
	expectedHash := strings.TrimPrefix(signatureHeader, "sha256=")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	computedHash := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(computedHash), []byte(expectedHash))
}
