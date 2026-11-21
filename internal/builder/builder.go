package builder

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/valkyrie-stack/bifrost/internal/config"
	"github.com/valkyrie-stack/bifrost/internal/domain"
	"github.com/valkyrie-stack/bifrost/internal/proxy"
	"github.com/valkyrie-stack/bifrost/internal/runtime"
	"github.com/valkyrie-stack/bifrost/internal/store"
	"github.com/valkyrie-stack/bifrost/internal/sysguard"
)

// Builder orchestrates deployments and builds with strict memory guards.
type Builder struct {
	cfg       *config.Config
	store     *store.Store
	rtClient  *runtime.ContainerClient
	proxy     *proxy.Proxy
	buildSem  chan struct{} // Global semaphore: 1 concurrent build on <= 1GB RAM hosts (ADR-004)
	logMu     sync.RWMutex
	logBuffer map[string][]string // In-memory ring buffer for active deployments
}

// NewBuilder initializes the build orchestrator.
func NewBuilder(cfg *config.Config, s *store.Store, rtClient *runtime.ContainerClient, p *proxy.Proxy) *Builder {
	maxConcurrent := cfg.Build.MaxConcurrentBuilds
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	return &Builder{
		cfg:       cfg,
		store:     s,
		rtClient:  rtClient,
		proxy:     p,
		buildSem:  make(chan struct{}, maxConcurrent),
		logBuffer: make(map[string][]string),
	}
}

// AppendLog writes a log line to file and in-memory buffer for real-time streaming.
func (b *Builder) AppendLog(deploymentID, line string) {
	b.logMu.Lock()
	defer b.logMu.Unlock()

	// Keep max 1000 lines in memory
	logs := b.logBuffer[deploymentID]
	if len(logs) > 1000 {
		logs = logs[1:]
	}
	b.logBuffer[deploymentID] = append(logs, line)

	// Append to file
	logFile := filepath.Join(b.cfg.Server.DataDir, "logs", fmt.Sprintf("%s.log", deploymentID))
	_ = os.MkdirAll(filepath.Dir(logFile), 0755)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		defer f.Close()
		_, _ = f.WriteString(line + "\n")
	}
}

// GetLogs returns the current buffered logs for a deployment.
func (b *Builder) GetLogs(deploymentID string) []string {
	b.logMu.RLock()
	defer b.logMu.RUnlock()

	if logs, ok := b.logBuffer[deploymentID]; ok {
		cpy := make([]string, len(logs))
		copy(cpy, logs)
		return cpy
	}

	// Fallback to reading log file on disk
	logFile := filepath.Join(b.cfg.Server.DataDir, "logs", fmt.Sprintf("%s.log", deploymentID))
	data, err := os.ReadFile(logFile)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// DeployArtifact extracts a pre-built tarball from GitHub Actions (ADR-004: 0MB Server Build RAM).
func (b *Builder) DeployArtifact(ctx context.Context, project *domain.Project, tarball io.Reader, dep *domain.Deployment) error {
	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Starting artifact deployment for %s", project.Name))
	b.AppendLog(dep.ID, "[Bifrost] Memory Strategy: Remote GitHub Actions Build (0MB server build RAM)")

	dep.Status = domain.StatusBuilding
	_ = b.store.SaveDeployment(dep)

	releaseID := time.Now().Format("20060102150405")
	releaseDir := filepath.Join(b.cfg.Server.DataDir, "projects", project.ID, "releases", releaseID)
	if err := os.MkdirAll(releaseDir, 0755); err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to create release directory: %v", err))
	}

	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Extracting artifact into %s", releaseDir))
	if err := extractTarGz(tarball, releaseDir); err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to extract artifact: %v", err))
	}

	// Update current symlink
	currentLink := filepath.Join(b.cfg.Server.DataDir, "projects", project.ID, "current")
	_ = os.Remove(currentLink)
	_ = os.Symlink(releaseDir, currentLink)

	b.AppendLog(dep.ID, "[Bifrost] Artifact extracted successfully")

	// Handle Static Projects (ADR-006: Zero-Container Hosting)
	if project.BuildType == domain.BuildTypeStatic {
		staticTargetDir := currentLink
		if project.OutputDir != "" {
			sub := filepath.Join(currentLink, project.OutputDir)
			if _, err := os.Stat(sub); err == nil {
				staticTargetDir = sub
			}
		}

		b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Configured Static Direct Serve from %s (0MB container RAM)", staticTargetDir))
		if err := b.updateRoutes(project, true, staticTargetDir, ""); err != nil {
			return b.failDeployment(dep, fmt.Sprintf("failed to configure routes: %v", err))
		}

		return b.completeDeployment(dep, project, "")
	}

	// Handle Containerized Projects (Docker/Node)
	return b.deployContainerFromDirectory(ctx, project, dep, currentLink)
}

func (b *Builder) deployContainerFromDirectory(ctx context.Context, project *domain.Project, dep *domain.Deployment, dir string) error {
	dep.Status = domain.StatusDeploying
	_ = b.store.SaveDeployment(dep)

	imageTag := fmt.Sprintf("bifrost/%s:%s", project.ID, dep.ID[:8])
	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Building container image: %s", imageTag))

	// Run container build using Kiri/Docker CLI or socket
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", imageTag, dir)
	if b.rtClient.EngineType() == "kiri" {
		if _, err := exec.LookPath("kiri"); err == nil {
			cmd = exec.CommandContext(ctx, "kiri", "build", "-t", imageTag, dir)
		}
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to start container build: %v", err))
	}

	// Stream logs
	go b.streamScanner(dep.ID, stdout)
	go b.streamScanner(dep.ID, stderr)

	if err := cmd.Wait(); err != nil {
		return b.failDeployment(dep, fmt.Sprintf("container build failed: %v", err))
	}

	// Create and start new container
	containerName := fmt.Sprintf("bifrost-%s-%s", project.ID, dep.ID[:8])
	targetPort := project.Port
	if targetPort <= 0 {
		targetPort = 3000
	}

	memLimit := project.MemoryLimitMB
	if memLimit <= 0 {
		memLimit = b.cfg.Build.DefaultMemoryLimitMB
	}

	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Creating container %s (Memory Limit: %dMB)", containerName, memLimit))
	contID, err := b.rtClient.CreateContainer(ctx, runtime.ContainerConfig{
		Name:          containerName,
		Image:         imageTag,
		TargetPort:    targetPort,
		MemoryLimitMB: memLimit,
		Env:           project.EnvVars,
	})
	if err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to create container: %v", err))
	}

	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Starting container %s", contID[:12]))
	if err := b.rtClient.StartContainer(ctx, contID); err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to start container: %v", err))
	}

	// Inspect mapped host port
	inspect, err := b.rtClient.InspectContainer(ctx, contID)
	if err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to inspect container: %v", err))
	}

	hostPort, err := b.rtClient.GetMappedPort(inspect, targetPort)
	if err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to resolve mapped port: %v", err))
	}

	upstreamURL := fmt.Sprintf("http://127.0.0.1:%d", hostPort)
	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Application running at %s", upstreamURL))

	// Health check ping before switching routes (Zero-downtime cutover)
	if err := b.waitForHealthy(ctx, upstreamURL, 15*time.Second); err != nil {
		_ = b.rtClient.StopContainer(ctx, contID, 5)
		_ = b.rtClient.RemoveContainer(ctx, contID)
		return b.failDeployment(dep, fmt.Sprintf("health check failed: %v", err))
	}

	// Atomically switch routes
	if err := b.updateRoutes(project, false, "", upstreamURL); err != nil {
		return b.failDeployment(dep, fmt.Sprintf("failed to update routes: %v", err))
	}

	// Stop previous container if any
	if project.ActiveContainerID != "" && project.ActiveContainerID != contID {
		b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Stopping previous container %s", project.ActiveContainerID[:12]))
		go func(oldID string) {
			_ = b.rtClient.StopContainer(context.Background(), oldID, 10)
			_ = b.rtClient.RemoveContainer(context.Background(), oldID)
		}(project.ActiveContainerID)
	}

	return b.completeDeployment(dep, project, contID)
}

// QueueLocalBuild handles Git clone and memory-capped in-situ build (ADR-004 fallback).
func (b *Builder) QueueLocalBuild(ctx context.Context, project *domain.Project, dep *domain.Deployment) {
	go func() {
		b.AppendLog(dep.ID, "[Bifrost] Waiting for global build lock (Max 1 concurrent build on <= 1GB RAM)...")
		select {
		case b.buildSem <- struct{}{}:
			defer func() { <-b.buildSem }()
		case <-ctx.Done():
			_ = b.failDeployment(dep, "build cancelled while waiting in queue")
			return
		}

		b.executeLocalBuild(ctx, project, dep)
	}()
}

func (b *Builder) executeLocalBuild(ctx context.Context, project *domain.Project, dep *domain.Deployment) {
	dep.Status = domain.StatusBuilding
	_ = b.store.SaveDeployment(dep)

	// Check Host Resources
	metrics := sysguard.GetSystemMetrics(0, 0)
	if metrics.TotalRAMMB <= 1100 && metrics.TotalSwapMB < 1500 {
		b.AppendLog(dep.ID, "[!] Low swap on low-RAM host. Memory-constrained build parameters active.")
	}

	workDir := filepath.Join(b.cfg.Server.DataDir, "projects", project.ID, "src")
	_ = os.MkdirAll(workDir, 0755)

	// Git Clone or Pull
	if _, err := os.Stat(filepath.Join(workDir, ".git")); os.IsNotExist(err) {
		b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Cloning %s (%s)...", project.RepoURL, project.Branch))
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "-b", project.Branch, project.RepoURL, workDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = b.failDeployment(dep, fmt.Sprintf("git clone failed: %v: %s", err, string(out)))
			return
		}
	} else {
		b.AppendLog(dep.ID, "[Bifrost] Fetching latest commit...")
		cmd := exec.CommandContext(ctx, "git", "-C", workDir, "pull", "origin", project.Branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = b.failDeployment(dep, fmt.Sprintf("git pull failed: %v: %s", err, string(out)))
			return
		}
	}

	appDir := workDir
	if project.RootDir != "" {
		appDir = filepath.Join(workDir, project.RootDir)
	}

	// In-Situ Build with V8 Memory Caps (ADR-004)
	if project.InstallCmd != "" {
		b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Running install: %s", project.InstallCmd))
		if err := b.runCommandWithMemoryCaps(ctx, dep.ID, appDir, project.InstallCmd); err != nil {
			_ = b.failDeployment(dep, fmt.Sprintf("install failed: %v", err))
			return
		}
	}

	if project.BuildCmd != "" {
		b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Running build: %s", project.BuildCmd))
		if err := b.runCommandWithMemoryCaps(ctx, dep.ID, appDir, project.BuildCmd); err != nil {
			_ = b.failDeployment(dep, fmt.Sprintf("build failed: %v", err))
			return
		}
	}

	if project.BuildType == domain.BuildTypeStatic {
		outputDir := filepath.Join(appDir, "dist")
		if project.OutputDir != "" {
			outputDir = filepath.Join(appDir, project.OutputDir)
		}
		b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Static Direct Serve active from %s", outputDir))
		_ = b.updateRoutes(project, true, outputDir, "")
		_ = b.completeDeployment(dep, project, "")
		return
	}

	// Container build
	_ = b.deployContainerFromDirectory(ctx, project, dep, appDir)
}

func (b *Builder) runCommandWithMemoryCaps(ctx context.Context, depID, dir, commandStr string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", commandStr)
	cmd.Dir = dir

	// Inject ADR-004 Memory Capping Environment Variables
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("NODE_OPTIONS=--max-old-space-size=%d", b.cfg.Build.NodeMaxOldSpaceMB),
		fmt.Sprintf("NPM_CONFIG_JOBS=%d", b.cfg.Build.NpmConfigJobs),
		"UV_THREADPOOL_SIZE=2",
	)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}

	go b.streamScanner(depID, stdout)
	go b.streamScanner(depID, stderr)

	return cmd.Wait()
}

func (b *Builder) streamScanner(depID string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		b.AppendLog(depID, scanner.Text())
	}
}

func (b *Builder) waitForHealthy(ctx context.Context, targetURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 500 {
					return nil // Target upstream responded
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timeout waiting for %s to become healthy", targetURL)
}

func (b *Builder) updateRoutes(project *domain.Project, isStatic bool, staticPath, upstreamURL string) error {
	_ = b.store.DeleteRoutesByProject(project.ID)

	for _, domainName := range project.Domains {
		route := &domain.RouteTarget{
			Domain:      domainName,
			ProjectID:   project.ID,
			IsStatic:    isStatic,
			StaticPath:  staticPath,
			UpstreamURL: upstreamURL,
		}
		if err := b.store.SaveRoute(route); err != nil {
			return err
		}
	}

	// Refresh proxy routing table
	allRoutes, err := b.store.ListRoutes()
	if err == nil {
		b.proxy.UpdateRoutes(allRoutes)
	}
	return nil
}

func (b *Builder) completeDeployment(dep *domain.Deployment, project *domain.Project, containerID string) error {
	now := time.Now().UTC()
	dep.Status = domain.StatusHealthy
	dep.ContainerID = containerID
	dep.FinishedAt = &now
	_ = b.store.SaveDeployment(dep)

	project.ActiveDeploymentID = dep.ID
	project.ActiveContainerID = containerID
	_ = b.store.SaveProject(project)

	b.AppendLog(dep.ID, "=========================================")
	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] Deployment %s succeeded! All routes active.", dep.ID[:8]))
	b.AppendLog(dep.ID, "=========================================")
	return nil
}

func (b *Builder) failDeployment(dep *domain.Deployment, reason string) error {
	now := time.Now().UTC()
	dep.Status = domain.StatusFailed
	dep.FinishedAt = &now
	_ = b.store.SaveDeployment(dep)

	b.AppendLog(dep.ID, fmt.Sprintf("[Bifrost] ERROR: %s", reason))
	log.Printf("[Builder] Deployment %s failed: %s", dep.ID, reason)
	return fmt.Errorf("%s", reason)
}

func extractTarGz(gzipStream io.Reader, targetDir string) error {
	uncompressed, err := gzip.NewReader(gzipStream)
	if err != nil {
		return err
	}
	defer uncompressed.Close()

	tarReader := tar.NewReader(uncompressed)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		cleanPath := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
			continue // Prevent Zip Slip vulnerability
		}

		destPath := filepath.Join(targetDir, cleanPath)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}
	return nil
}

// GenerateID produces a random slug identifier for projects and deployments.
func GenerateID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
}
