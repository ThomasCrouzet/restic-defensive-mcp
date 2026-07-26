// Package repositories holds the sealed registry of configured restic repositories.
package repositories

import (
	"context"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/config"
	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

// Entry is one sealed repository handle. Secret paths and backend credentials
// remain encapsulated in childEnv and are never exposed through this type.
type Entry struct {
	ID      string
	Label   string
	Backend restic.BackendKind
	Hosts   policy.HostPolicy
	Tags    policy.TagPolicy
	Paths   policy.PathPolicy

	sem      chan struct{}
	childEnv []string // built once at registration (includes backend env; not re-exported)
}

// PublicInfo is safe to return to MCP clients.
type PublicInfo struct {
	ID           string   `json:"id"`
	Label        string   `json:"label,omitempty"`
	Backend      string   `json:"backend"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	AllowedTags  []string `json:"allowed_tags,omitempty"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
}

// Registry is the frozen set of repositories after boot.
type Registry struct {
	entries map[string]*Entry
	order   []string
	limits  config.Limits
	runner  restic.Runner
	audit   *audit.Logger
	global  chan struct{}
}

// New builds a registry from validated config. It classifies backends and builds child envs.
func New(cfg *config.Config, runner restic.Runner, log *audit.Logger) (*Registry, error) {
	r := &Registry{
		entries: make(map[string]*Entry, len(cfg.Repositories)),
		limits:  cfg.Limits,
		runner:  runner,
		audit:   log,
		global:  make(chan struct{}, cfg.Limits.MaxConcurrentGlobal),
	}
	for _, rc := range cfg.Repositories {
		loc, err := config.ReadRepositoryLocation(rc.RepositoryFile)
		if err != nil {
			return nil, err
		}
		// Validate password file exists and is well-formed without retaining content.
		if _, err := config.OpenSecretFile(rc.PasswordFile); err != nil {
			return nil, err
		}
		kind := restic.ClassifyBackend(loc)
		if err := restic.EnsureSupported(kind); err != nil {
			return nil, err
		}
		var extra []string
		for _, ef := range rc.EnvFiles {
			pairs, err := restic.LoadEnvFile(ef, config.OpenSecretFile)
			if err != nil {
				return nil, err
			}
			extra = append(extra, pairs...)
		}
		childEnv, err := restic.BuildChildEnv(restic.ChildEnvOpts{
			Repository:   loc,
			PasswordFile: rc.PasswordFile,
			CacheDir:     rc.CacheDir,
			HomeDir:      "/var/empty",
			ExtraEnv:     extra,
		})
		if err != nil {
			return nil, err
		}

		e := &Entry{
			ID:       rc.ID,
			Label:    rc.Label,
			Backend:  kind,
			Hosts:    policy.NewHostPolicy(rc.AllowedHosts),
			Tags:     policy.NewTagPolicy(rc.AllowedTags),
			Paths:    policy.PathPolicy{Allowed: append([]string(nil), rc.AllowedPaths...)},
			sem:      make(chan struct{}, cfg.Limits.MaxConcurrentPerRepo),
			childEnv: childEnv,
		}
		r.entries[e.ID] = e
		r.order = append(r.order, e.ID)
	}
	return r, nil
}

// Get returns an entry by id.
func (r *Registry) Get(id string) (*Entry, error) {
	e, ok := r.entries[id]
	if !ok {
		return nil, apperr.New(apperr.RepositoryNotFound, "unknown repository_id")
	}
	return e, nil
}

// List returns public info for all repositories in config order.
func (r *Registry) List() []PublicInfo {
	out := make([]PublicInfo, 0, len(r.order))
	for _, id := range r.order {
		e := r.entries[id]
		out = append(out, PublicInfo{
			ID:           e.ID,
			Label:        e.Label,
			Backend:      string(e.Backend),
			AllowedHosts: append([]string(nil), e.Hosts.Allowed...),
			AllowedTags:  append([]string(nil), e.Tags.Allowed...),
			AllowedPaths: append([]string(nil), e.Paths.Allowed...),
		})
	}
	return out
}

// Limits returns a copy of runtime limits.
func (r *Registry) Limits() config.Limits { return r.limits }

// IDs returns configured repository ids.
func (r *Registry) IDs() []string {
	return append([]string(nil), r.order...)
}

// Run executes a restic command against a repository with concurrency limits.
func (r *Registry) Run(ctx context.Context, repoID string, argv []string, expensive bool) (*restic.Result, error) {
	e, err := r.Get(repoID)
	if err != nil {
		return nil, err
	}
	if err := restic.AssertArgvAllowed(argv); err != nil {
		return nil, apperr.New(apperr.InternalError, "disallowed argv").WithCause(err)
	}

	// Acquire the repository slot first. Otherwise, several requests queued on
	// one busy repository could consume every global slot and starve other
	// repositories without running any work.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, apperr.New(apperr.Cancelled, "request cancelled")
	}

	// Global concurrency
	select {
	case r.global <- struct{}{}:
		defer func() { <-r.global }()
	case <-ctx.Done():
		return nil, apperr.New(apperr.Cancelled, "request cancelled")
	}

	timeout := r.limits.CommandTimeout
	if expensive {
		timeout = r.limits.ExpensiveCommandTimeout
	}

	start := time.Now()
	res, err := r.runner.Run(ctx, restic.RunRequest{
		Argv:           argv,
		Env:            e.childEnv,
		Timeout:        timeout,
		MaxOutputBytes: r.limits.MaxOutputBytes,
	})
	status := "ok"
	code := ""
	if err != nil {
		status = "error"
		if ae, ok := apperr.As(err); ok {
			code = string(ae.Code)
			if ae.Code == apperr.NotAllowed {
				status = "denied"
			}
		}
	}
	if r.audit != nil {
		r.audit.Log(audit.Event{
			Action:       "restic_run",
			RepositoryID: repoID,
			Status:       status,
			ErrorCode:    code,
			DurationMS:   time.Since(start).Milliseconds(),
			Message:      argv[0],
		})
	}
	return res, err
}
