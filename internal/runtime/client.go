package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// ContainerClient interacts with Kiri or Docker engine over a Unix domain socket.
// ADR-002: Implemented without external Docker SDK to minimize memory and binary bloat.
type ContainerClient struct {
	client     *http.Client
	socketPath string
	engineType string // "kiri", "docker", or "podman"
}

// ContainerConfig holds parameters for creating a new container.
type ContainerConfig struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Cmd           []string          `json:"cmd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	TargetPort    int               `json:"target_port"`     // Internal port (e.g. 3000)
	MemoryLimitMB int64             `json:"memory_limit_mb"` // cgroup memory limit in MB
	Labels        map[string]string `json:"labels,omitempty"`
}

// ContainerInspectResponse captures inspect details needed by Bifrost.
type ContainerInspectResponse struct {
	ID    string `json:"Id"`
	State struct {
		Status   string `json:"Status"` // "running", "exited", etc.
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Ports     map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// NewContainerClient creates a new client, automatically detecting Kiri or Docker.
func NewContainerClient(socketPath string, autoDetect bool) (*ContainerClient, error) {
	if autoDetect || socketPath == "" {
		socketPath = detectSocket()
	}

	engineType := "docker"
	if _, err := os.Stat("/var/run/kiri.sock"); err == nil && socketPath == "/var/run/kiri.sock" {
		engineType = "kiri"
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives:     false,
		MaxIdleConns:          5,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
	}

	return &ContainerClient{
		client:     client,
		socketPath: socketPath,
		engineType: engineType,
	}, nil
}

func detectSocket() string {
	candidates := []string{
		"/var/run/kiri.sock",         // Kiri (recommended ultra-low RAM engine)
		"/var/run/docker.sock",       // Standard Docker
		"/run/podman/podman.sock",    // Podman system socket
		"/tmp/kiri.sock",
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "/var/run/docker.sock" // Default fallback
}

// EngineType returns whether the active engine is kiri or docker.
func (c *ContainerClient) EngineType() string {
	return c.engineType
}

// SocketPath returns the active unix socket path.
func (c *ContainerClient) SocketPath() string {
	return c.socketPath
}

// Ping checks connectivity with the container daemon.
func (c *ContainerClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("container daemon ping failed (%s): %w", c.socketPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected ping status: %d", resp.StatusCode)
	}
	return nil
}

// CreateContainer creates a container with memory ceilings and port mapping.
func (c *ContainerClient) CreateContainer(ctx context.Context, cfg ContainerConfig) (string, error) {
	var envList []string
	for k, v := range cfg.Env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	portKey := fmt.Sprintf("%d/tcp", cfg.TargetPort)

	// Memory in bytes (ADR-002: strictly injected cgroup limits)
	memBytes := cfg.MemoryLimitMB * 1024 * 1024
	if memBytes <= 0 {
		memBytes = 256 * 1024 * 1024 // 256MB default safe ceiling
	}

	payload := map[string]any{
		"Image": cfg.Image,
		"Cmd":   cfg.Cmd,
		"Env":   envList,
		"ExposedPorts": map[string]any{
			portKey: struct{}{},
		},
		"HostConfig": map[string]any{
			"Memory":            memBytes,
			"MemoryReservation": memBytes * 3 / 4, // Soft limit at 75%
			"OomKillDisable":    false,            // Ensure host survives
			"PortBindings": map[string]any{
				portKey: []map[string]string{
					{"HostIp": "127.0.0.1", "HostPort": ""}, // Random host port on 127.0.0.1
				},
			},
			"RestartPolicy": map[string]any{
				"Name":              "unless-stopped",
				"MaximumRetryCount": 3,
			},
		},
		"Labels": cfg.Labels,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("http://localhost/containers/create?name=%s", cfg.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create container returned %d: %s", resp.StatusCode, string(respBody))
	}

	var res struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &res); err != nil {
		return "", err
	}
	return res.ID, nil
}

// StartContainer boots a container by ID.
func (c *ContainerClient) StartContainer(ctx context.Context, containerID string) error {
	url := fmt.Sprintf("http://localhost/containers/%s/start", containerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("start container returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// StopContainer stops a container with a graceful timeout.
func (c *ContainerClient) StopContainer(ctx context.Context, containerID string, timeoutSec int) error {
	url := fmt.Sprintf("http://localhost/containers/%s/stop?t=%d", containerID, timeoutSec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stop container returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// RemoveContainer deletes a container.
func (c *ContainerClient) RemoveContainer(ctx context.Context, containerID string) error {
	url := fmt.Sprintf("http://localhost/containers/%s?force=true&v=true", containerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("remove container: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remove container returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// InspectContainer inspects state, networking, and mapped host port.
func (c *ContainerClient) InspectContainer(ctx context.Context, containerID string) (*ContainerInspectResponse, error) {
	url := fmt.Sprintf("http://localhost/containers/%s/json", containerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("inspect container: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("inspect container returned %d: %s", resp.StatusCode, string(b))
	}

	var res ContainerInspectResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetMappedPort extracts the 127.0.0.1 host port assigned to the container.
func (c *ContainerClient) GetMappedPort(inspect *ContainerInspectResponse, targetPort int) (int, error) {
	portKey := fmt.Sprintf("%d/tcp", targetPort)
	bindings, ok := inspect.NetworkSettings.Ports[portKey]
	if !ok || len(bindings) == 0 {
		return 0, fmt.Errorf("no port mapping found for %s", portKey)
	}
	hostPort, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil {
		return 0, fmt.Errorf("invalid host port %q: %w", bindings[0].HostPort, err)
	}
	return hostPort, nil
}

// ContainerLogs returns a reader with stdout and stderr output.
func (c *ContainerClient) ContainerLogs(ctx context.Context, containerID string, tail string) (io.ReadCloser, error) {
	if tail == "" {
		tail = "100"
	}
	url := fmt.Sprintf("http://localhost/containers/%s/logs?stdout=1&stderr=1&tail=%s", containerID, tail)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get container logs: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("get logs returned %d: %s", resp.StatusCode, string(b))
	}
	return resp.Body, nil
}

// CountRunningContainers returns the number of active running containers.
func (c *ContainerClient) CountRunningContainers(ctx context.Context) (int, error) {
	url := "http://localhost/containers/json?filters=%7B%22status%22%3A%5B%22running%22%5D%7D"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var containers []any
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return 0, err
	}
	return len(containers), nil
}
