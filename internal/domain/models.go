package domain

import (
	"time"
)

// BuildType determines how Bifrost handles deployment and runtime.
type BuildType string

const (
	BuildTypeStatic     BuildType = "static"     // Zero-container static direct serve
	BuildTypeDockerfile BuildType = "dockerfile" // Containerized build using Kiri/Docker
	BuildTypeNode       BuildType = "node"       // Node.js application
)

// DeploymentStatus traces the lifecycle of a deployment.
type DeploymentStatus string

const (
	StatusQueued    DeploymentStatus = "queued"
	StatusBuilding  DeploymentStatus = "building"
	StatusDeploying DeploymentStatus = "deploying"
	StatusHealthy   DeploymentStatus = "healthy"
	StatusFailed    DeploymentStatus = "failed"
	StatusCancelled DeploymentStatus = "cancelled"
)

// DeploymentTrigger identifies how the build was started.
type DeploymentTrigger string

const (
	TriggerGitHubWebhook DeploymentTrigger = "github_webhook"
	TriggerArtifactPush  DeploymentTrigger = "github_actions_artifact"
	TriggerManual        DeploymentTrigger = "manual"
)

// Project represents an application managed by Bifrost.
type Project struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	RepoURL            string            `json:"repo_url"`
	Branch             string            `json:"branch"`
	BuildType          BuildType         `json:"build_type"`
	RootDir            string            `json:"root_dir,omitempty"` // For monorepos
	InstallCmd         string            `json:"install_cmd,omitempty"`
	BuildCmd           string            `json:"build_cmd,omitempty"`
	OutputDir          string            `json:"output_dir,omitempty"` // e.g. "dist" for static sites
	Port               int               `json:"port"`                 // Container listening port (e.g. 3000)
	MemoryLimitMB      int64             `json:"memory_limit_mb"`      // e.g. 256MB
	Domains            []string          `json:"domains"`              // e.g. ["app.example.com"]
	EnvVars            map[string]string `json:"env_vars,omitempty"`
	WebhookSecret      string            `json:"webhook_secret,omitempty"` // GitHub HMAC secret
	DeployToken        string            `json:"deploy_token,omitempty"`   // Token for GitHub Actions artifact push
	ActiveDeploymentID string            `json:"active_deployment_id,omitempty"`
	ActiveContainerID  string            `json:"active_container_id,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

// Deployment represents a discrete deployment run for a project.
type Deployment struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"project_id"`
	CommitHash  string            `json:"commit_hash,omitempty"`
	CommitMsg   string            `json:"commit_msg,omitempty"`
	Author      string            `json:"author,omitempty"`
	Status      DeploymentStatus  `json:"status"`
	Trigger     DeploymentTrigger `json:"trigger"`
	ArtifactURL string            `json:"artifact_url,omitempty"`
	ContainerID string            `json:"container_id,omitempty"`
	LogsPath    string            `json:"logs_path,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  *time.Time        `json:"finished_at,omitempty"`
}

// RouteTarget defines routing destinations for the in-process reverse proxy.
type RouteTarget struct {
	Domain      string `json:"domain"`
	IsStatic    bool   `json:"is_static"`
	StaticPath  string `json:"static_path,omitempty"`  // Local filesystem directory if static
	UpstreamURL string `json:"upstream_url,omitempty"` // "http://127.0.0.1:PORT" if containerized
	ProjectID   string `json:"project_id"`
}

// SystemMetrics captures live host telemetry and Bifrost runtime resource consumption.
type SystemMetrics struct {
	// Host Telemetry
	TotalRAMMB       uint64  `json:"total_ram_mb"`
	UsedRAMMB        uint64  `json:"used_ram_mb"`
	FreeRAMMB        uint64  `json:"free_ram_mb"`
	AvailableRAMMB   uint64  `json:"available_ram_mb"`
	RAMUsagePercent  float64 `json:"ram_usage_percent"`
	TotalSwapMB      uint64  `json:"total_swap_mb"`
	UsedSwapMB       uint64  `json:"used_swap_mb"`
	FreeSwapMB       uint64  `json:"free_swap_mb"`
	SwapUsagePercent float64 `json:"swap_usage_percent"`
	DiskTotalGB      float64 `json:"disk_total_gb"`
	DiskUsedGB       float64 `json:"disk_used_gb"`
	DiskFreeGB       float64 `json:"disk_free_gb"`
	DiskUsagePercent float64 `json:"disk_usage_percent"`
	CPUUsagePercent  float64 `json:"cpu_usage_percent"`
	CPUCores         int     `json:"cpu_cores"`

	// Bifrost Self-Resource Footprint
	ProcessRSSMB     uint64  `json:"process_rss_mb"` // Real OS Resident memory used by Bifrost
	GoAllocMB        uint64  `json:"go_alloc_mb"`    // Heap in active use
	GoSysMB          uint64  `json:"go_sys_mb"`      // Virtual memory obtained from OS
	NumGoroutines    int     `json:"num_goroutines"`
	UptimeSeconds    int64   `json:"uptime_seconds"`
	ActiveContainers int     `json:"active_containers"`
	ActiveRoutes     int     `json:"active_routes"`
	OS               string  `json:"os"`
	Arch             string  `json:"arch"`
	Hostname         string  `json:"hostname"`
}
