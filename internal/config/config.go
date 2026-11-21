package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the complete runtime configuration for Bifrost.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
	Build    BuildConfig    `yaml:"build"`
	Security SecurityConfig `yaml:"security"`
}

// ServerConfig manages HTTP/HTTPS listening, domains, and ACME TLS.
type ServerConfig struct {
	HTTPPort     int      `yaml:"http_port"`     // Default: 80
	HTTPSPort    int      `yaml:"https_port"`    // Default: 443
	DashboardPort int     `yaml:"dashboard_port"`// Default: 8080 (or unified on :80/:443)
	DataDir      string   `yaml:"data_dir"`      // Default: /var/lib/bifrost
	HostDomain   string   `yaml:"host_domain"`   // e.g. "bifrost.mydomain.com"
	AcmeEmail    string   `yaml:"acme_email"`    // Email for Let's Encrypt certificates
	EnableTLS    bool     `yaml:"enable_tls"`    // Whether to activate autocert
	AllowedHosts []string `yaml:"allowed_hosts"` // Domains allowed for autocert
}

// RuntimeConfig manages communication with Kiri or Docker engine.
type RuntimeConfig struct {
	SocketPath    string        `yaml:"socket_path"`    // Path to Kiri or Docker unix socket
	AutoDetect    bool          `yaml:"auto_detect"`    // Auto-detect /var/run/kiri.sock vs docker.sock
	ClientTimeout time.Duration `yaml:"client_timeout"` // Default: 30s
}

// BuildConfig controls resource constraints and concurrency during builds.
type BuildConfig struct {
	MaxConcurrentBuilds int    `yaml:"max_concurrent_builds"` // Default: 1 (strictly enforced on <=1GB RAM)
	NodeMaxOldSpaceMB   int    `yaml:"node_max_old_space_mb"`  // Default: 384
	DefaultMemoryLimitMB int64 `yaml:"default_memory_limit_mb"`// Default: 256MB per container
	NpmConfigJobs       int    `yaml:"npm_config_jobs"`        // Default: 1
}

// SecurityConfig controls authentication tokens and webhooks.
type SecurityConfig struct {
	AdminPasswordHash string `yaml:"admin_password_hash"`
	JWTSecret         string `yaml:"jwt_secret"`
	WebhookSecret     string `yaml:"webhook_secret"`
}

// DefaultConfig returns an optimized configuration tailored for 1GB RAM hosts.
func DefaultConfig() *Config {
	homeDir, _ := os.UserHomeDir()
	defaultDataDir := filepath.Join(homeDir, ".bifrost")
	if os.Geteuid() == 0 {
		defaultDataDir = "/var/lib/bifrost"
	}

	return &Config{
		Server: ServerConfig{
			HTTPPort:      80,
			HTTPSPort:     443,
			DashboardPort: 8080,
			DataDir:       defaultDataDir,
			EnableTLS:     false,
		},
		Runtime: RuntimeConfig{
			SocketPath:    "/var/run/kiri.sock",
			AutoDetect:    true,
			ClientTimeout: 45 * time.Second,
		},
		Build: BuildConfig{
			MaxConcurrentBuilds:  1,
			NodeMaxOldSpaceMB:    384,
			DefaultMemoryLimitMB: 256,
			NpmConfigJobs:        1,
		},
		Security: SecurityConfig{
			JWTSecret: "change-me-in-production-bifrost-secret-key-32b",
		},
	}
}

// LoadConfig loads configuration from path or creates a default one.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config file: %w", err)
			}
		} else {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse yaml config: %w", err)
			}
		}
	}

	// Environment variable overrides
	if val := os.Getenv("BIFROST_DATA_DIR"); val != "" {
		cfg.Server.DataDir = val
	}
	if val := os.Getenv("BIFROST_HTTP_PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil {
			cfg.Server.HTTPPort = p
		}
	}
	if val := os.Getenv("BIFROST_HTTPS_PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil {
			cfg.Server.HTTPSPort = p
		}
	}
	if val := os.Getenv("BIFROST_DASHBOARD_PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil {
			cfg.Server.DashboardPort = p
		}
	}
	if val := os.Getenv("BIFROST_HOST_DOMAIN"); val != "" {
		cfg.Server.HostDomain = val
	}
	if val := os.Getenv("BIFROST_ACME_EMAIL"); val != "" {
		cfg.Server.AcmeEmail = val
		cfg.Server.EnableTLS = true
	}
	if val := os.Getenv("BIFROST_SOCKET_PATH"); val != "" {
		cfg.Runtime.SocketPath = val
		cfg.Runtime.AutoDetect = false
	}
	if val := os.Getenv("BIFROST_JWT_SECRET"); val != "" {
		cfg.Security.JWTSecret = val
	}

	// Ensure directories exist
	if err := os.MkdirAll(cfg.Server.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Server.DataDir, "certs"), 0700); err != nil {
		return nil, fmt.Errorf("create certs directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Server.DataDir, "projects"), 0755); err != nil {
		return nil, fmt.Errorf("create projects directory: %w", err)
	}

	return cfg, nil
}
