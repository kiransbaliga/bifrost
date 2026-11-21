package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver (no CGO required)

	"github.com/valkyrie-stack/bifrost/internal/domain"
)

// Store provides persistence for Bifrost entities using embedded SQLite.
type Store struct {
	db *sql.DB
}

// NewStore initializes an embedded SQLite database configured with aggressive low-memory pragmas.
func NewStore(dataDir string) (*Store, error) {
	dbPath := filepath.Join(dataDir, "bifrost.db")

	// DSN with low-memory pragmas (ADR-005)
	// cache_size=-2000 caps the page cache at ~2MB RAM
	// mmap_size=0 stops SQLite from mapping large memory spaces
	// temp_store=FILE ensures temporary index/sort tables stay on disk
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=cache_size(-2000)&_pragma=synchronous(NORMAL)&_pragma=temp_store(FILE)&_pragma=mmap_size(0)&_pragma=busy_timeout(5000)",
		dbPath,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// Single connection pool is optimal for embedded SQLite on 1GB VPS
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS projects (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		repo_url TEXT NOT NULL,
		branch TEXT NOT NULL,
		build_type TEXT NOT NULL,
		root_dir TEXT,
		install_cmd TEXT,
		build_cmd TEXT,
		output_dir TEXT,
		port INTEGER NOT NULL,
		memory_limit_mb INTEGER NOT NULL,
		domains TEXT NOT NULL, -- JSON array
		env_vars TEXT NOT NULL, -- JSON object
		webhook_secret TEXT,
		deploy_token TEXT,
		active_deployment_id TEXT,
		active_container_id TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS deployments (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		commit_hash TEXT,
		commit_msg TEXT,
		author TEXT,
		status TEXT NOT NULL,
		trigger TEXT NOT NULL,
		artifact_url TEXT,
		container_id TEXT,
		logs_path TEXT,
		started_at DATETIME NOT NULL,
		finished_at DATETIME,
		FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_deployments_project ON deployments(project_id, started_at DESC);

	CREATE TABLE IF NOT EXISTS routes (
		domain TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		is_static BOOLEAN NOT NULL,
		static_path TEXT,
		upstream_url TEXT,
		FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
	);
	`
	_, err := s.db.Exec(schema)
	return err
}

// --- Project Operations ---

func (s *Store) SaveProject(p *domain.Project) error {
	domainsJSON, _ := json.Marshal(p.Domains)
	envJSON, _ := json.Marshal(p.EnvVars)

	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	query := `
	INSERT INTO projects (
		id, name, repo_url, branch, build_type, root_dir, install_cmd, build_cmd,
		output_dir, port, memory_limit_mb, domains, env_vars, webhook_secret, deploy_token,
		active_deployment_id, active_container_id, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name=excluded.name,
		repo_url=excluded.repo_url,
		branch=excluded.branch,
		build_type=excluded.build_type,
		root_dir=excluded.root_dir,
		install_cmd=excluded.install_cmd,
		build_cmd=excluded.build_cmd,
		output_dir=excluded.output_dir,
		port=excluded.port,
		memory_limit_mb=excluded.memory_limit_mb,
		domains=excluded.domains,
		env_vars=excluded.env_vars,
		webhook_secret=excluded.webhook_secret,
		deploy_token=excluded.deploy_token,
		active_deployment_id=excluded.active_deployment_id,
		active_container_id=excluded.active_container_id,
		updated_at=excluded.updated_at;
	`
	_, err := s.db.Exec(query,
		p.ID, p.Name, p.RepoURL, p.Branch, string(p.BuildType), p.RootDir, p.InstallCmd, p.BuildCmd,
		p.OutputDir, p.Port, p.MemoryLimitMB, string(domainsJSON), string(envJSON), p.WebhookSecret, p.DeployToken,
		p.ActiveDeploymentID, p.ActiveContainerID, p.CreatedAt, p.UpdatedAt,
	)
	return err
}

func (s *Store) GetProject(id string) (*domain.Project, error) {
	query := `SELECT id, name, repo_url, branch, build_type, root_dir, install_cmd, build_cmd,
		output_dir, port, memory_limit_mb, domains, env_vars, webhook_secret, deploy_token,
		active_deployment_id, active_container_id, created_at, updated_at FROM projects WHERE id = ?`

	row := s.db.QueryRow(query, id)
	return s.scanProject(row)
}

func (s *Store) ListProjects() ([]*domain.Project, error) {
	query := `SELECT id, name, repo_url, branch, build_type, root_dir, install_cmd, build_cmd,
		output_dir, port, memory_limit_mb, domains, env_vars, webhook_secret, deploy_token,
		active_deployment_id, active_container_id, created_at, updated_at FROM projects ORDER BY name ASC`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []*domain.Project
	for rows.Next() {
		p, err := s.scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *Store) DeleteProject(id string) error {
	_, err := s.db.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

type scannable interface {
	Scan(dest ...any) error
}

func (s *Store) scanProject(row scannable) (*domain.Project, error) {
	var p domain.Project
	var buildType, domainsJSON, envJSON string
	var actDepID, actContID sql.NullString
	var rootDir, installCmd, buildCmd, outputDir, hookSec, depTok sql.NullString

	err := row.Scan(
		&p.ID, &p.Name, &p.RepoURL, &p.Branch, &buildType, &rootDir, &installCmd, &buildCmd,
		&outputDir, &p.Port, &p.MemoryLimitMB, &domainsJSON, &envJSON, &hookSec, &depTok,
		&actDepID, &actContID, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	p.BuildType = domain.BuildType(buildType)
	p.RootDir = rootDir.String
	p.InstallCmd = installCmd.String
	p.BuildCmd = buildCmd.String
	p.OutputDir = outputDir.String
	p.WebhookSecret = hookSec.String
	p.DeployToken = depTok.String
	p.ActiveDeploymentID = actDepID.String
	p.ActiveContainerID = actContID.String

	_ = json.Unmarshal([]byte(domainsJSON), &p.Domains)
	_ = json.Unmarshal([]byte(envJSON), &p.EnvVars)

	return &p, nil
}

// --- Deployment Operations ---

func (s *Store) SaveDeployment(d *domain.Deployment) error {
	query := `
	INSERT INTO deployments (id, project_id, commit_hash, commit_msg, author, status, trigger, artifact_url, container_id, logs_path, started_at, finished_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		status=excluded.status,
		container_id=excluded.container_id,
		logs_path=excluded.logs_path,
		finished_at=excluded.finished_at;
	`
	_, err := s.db.Exec(query,
		d.ID, d.ProjectID, d.CommitHash, d.CommitMsg, d.Author, string(d.Status), string(d.Trigger),
		d.ArtifactURL, d.ContainerID, d.LogsPath, d.StartedAt, d.FinishedAt,
	)
	return err
}

func (s *Store) GetDeployment(id string) (*domain.Deployment, error) {
	query := `SELECT id, project_id, commit_hash, commit_msg, author, status, trigger, artifact_url, container_id, logs_path, started_at, finished_at
		FROM deployments WHERE id = ?`

	row := s.db.QueryRow(query, id)
	var d domain.Deployment
	var status, trigger string
	var ch, cm, auth, artURL, contID, lp sql.NullString

	err := row.Scan(&d.ID, &d.ProjectID, &ch, &cm, &auth, &status, &trigger, &artURL, &contID, &lp, &d.StartedAt, &d.FinishedAt)
	if err != nil {
		return nil, err
	}

	d.CommitHash = ch.String
	d.CommitMsg = cm.String
	d.Author = auth.String
	d.Status = domain.DeploymentStatus(status)
	d.Trigger = domain.DeploymentTrigger(trigger)
	d.ArtifactURL = artURL.String
	d.ContainerID = contID.String
	d.LogsPath = lp.String

	return &d, nil
}

func (s *Store) ListDeployments(projectID string, limit int) ([]*domain.Deployment, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT id, project_id, commit_hash, commit_msg, author, status, trigger, artifact_url, container_id, logs_path, started_at, finished_at
		FROM deployments WHERE project_id = ? ORDER BY started_at DESC LIMIT ?`

	rows, err := s.db.Query(query, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deps []*domain.Deployment
	for rows.Next() {
		var d domain.Deployment
		var status, trigger string
		var ch, cm, auth, artURL, contID, lp sql.NullString

		if err := rows.Scan(&d.ID, &d.ProjectID, &ch, &cm, &auth, &status, &trigger, &artURL, &contID, &lp, &d.StartedAt, &d.FinishedAt); err != nil {
			return nil, err
		}
		d.CommitHash = ch.String
		d.CommitMsg = cm.String
		d.Author = auth.String
		d.Status = domain.DeploymentStatus(status)
		d.Trigger = domain.DeploymentTrigger(trigger)
		d.ArtifactURL = artURL.String
		d.ContainerID = contID.String
		d.LogsPath = lp.String

		deps = append(deps, &d)
	}
	return deps, nil
}

// --- Route Operations ---

func (s *Store) SaveRoute(r *domain.RouteTarget) error {
	query := `
	INSERT INTO routes (domain, project_id, is_static, static_path, upstream_url)
	VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(domain) DO UPDATE SET
		project_id=excluded.project_id,
		is_static=excluded.is_static,
		static_path=excluded.static_path,
		upstream_url=excluded.upstream_url;
	`
	_, err := s.db.Exec(query, r.Domain, r.ProjectID, r.IsStatic, r.StaticPath, r.UpstreamURL)
	return err
}

func (s *Store) ListRoutes() ([]*domain.RouteTarget, error) {
	rows, err := s.db.Query("SELECT domain, project_id, is_static, static_path, upstream_url FROM routes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []*domain.RouteTarget
	for rows.Next() {
		var r domain.RouteTarget
		var sp, uu sql.NullString
		if err := rows.Scan(&r.Domain, &r.ProjectID, &r.IsStatic, &sp, &uu); err != nil {
			return nil, err
		}
		r.StaticPath = sp.String
		r.UpstreamURL = uu.String
		routes = append(routes, &r)
	}
	return routes, nil
}

func (s *Store) DeleteRoutesByProject(projectID string) error {
	_, err := s.db.Exec("DELETE FROM routes WHERE project_id = ?", projectID)
	return err
}
