// Package engine implements Sluice's git operations: sync (spec §12.1),
// promotion (§12.2), finalization detection (§12.3) and verification (§5.2).
//
// Structural security note (spec §9.2): the ONLY function that pushes to
// Gitea is Sync, and it pushes exclusively from the filter-repo output in a
// temporary directory. Promotion code paths (source-mirror / source-work)
// never receive the Gitea remote, so an unfiltered ref cannot reach Gitea
// by construction.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/geertarien/sluice/internal/execx"
)

// Bridge is the runtime view of a bridge: everything the engine needs,
// with secrets already decrypted by the caller.
type Bridge struct {
	Slug            string
	SourceRemoteURL string
	GiteaSSHURL     string
	ExcludedPaths   []string
	SyncBranches    []string
	SyncGlobs       []string
	TripwireStrings []string

	PromoteName         string
	PromoteEmail        string
	PromoteKeepTrailer  bool
	PromoteSignoff      bool
	PromoteBranchPrefix string // prefix for the pushed upstream branch (e.g. "ai/")
}

// PromotedBranchName is the upstream branch name for a promoted agent branch:
// <prefix><branch>, defaulting to "ai/" when no prefix is configured.
func (b *Bridge) PromotedBranchName(branch string) string {
	prefix := b.PromoteBranchPrefix
	if prefix == "" {
		prefix = "ai/"
	}
	return prefix + branch
}

// Engine executes git operations for one bridge within its workspace.
type Engine struct {
	Workdir    string // root directory holding one subdirectory per bridge slug
	KnownHosts string // managed known_hosts file; empty disables the override
	Runner     *execx.Runner
}

// New builds an engine. sshKeyPath, when non-empty, is a per-bridge private
// key file that ssh must use exclusively (IdentitiesOnly); when empty, ssh
// falls back to the container's default identity (e.g. a mounted
// ~/.ssh/id_ed25519).
func New(workdir, knownHosts, sshKeyPath string, runner *execx.Runner) *Engine {
	if runner.Env == nil && (knownHosts != "" || sshKeyPath != "") {
		ssh := "ssh"
		if sshKeyPath != "" {
			ssh += " -i " + sshKeyPath + " -o IdentitiesOnly=yes"
		}
		if knownHosts != "" {
			ssh += " -o UserKnownHostsFile=" + knownHosts + " -o StrictHostKeyChecking=yes"
		}
		runner.Env = append(runner.Env, "GIT_SSH_COMMAND="+ssh)
	}
	// Never let git prompt for credentials inside a job.
	runner.Env = append(runner.Env, "GIT_TERMINAL_PROMPT=0")
	return &Engine{Workdir: workdir, KnownHosts: knownHosts, Runner: runner}
}

// Workspace paths (spec §4).
func (e *Engine) ws(b *Bridge) string            { return filepath.Join(e.Workdir, b.Slug) }
func (e *Engine) sourceMirror(b *Bridge) string  { return filepath.Join(e.ws(b), "source-mirror") }
func (e *Engine) sourceWork(b *Bridge) string    { return filepath.Join(e.ws(b), "source-work") }
func (e *Engine) giteaClone(b *Bridge) string    { return filepath.Join(e.ws(b), "gitea-clone") }
func (e *Engine) commitMapPath(b *Bridge) string { return filepath.Join(e.ws(b), "commit-map") }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// InitWorkspace creates the per-bridge clones. The Gitea repo must already
// exist (the init job creates it via the API first) but may be empty; the
// gitea-clone is created lazily after the first sync has pushed content.
func (e *Engine) InitWorkspace(ctx context.Context, b *Bridge) error {
	if err := os.MkdirAll(e.ws(b), 0o755); err != nil {
		return err
	}
	if !exists(e.sourceMirror(b)) {
		if _, err := e.Runner.Run(ctx, e.ws(b), "git", "clone", "--mirror", "--", b.SourceRemoteURL, "source-mirror"); err != nil {
			return fmt.Errorf("clone source mirror: %w", err)
		}
	}
	if !exists(e.sourceWork(b)) {
		if _, err := e.Runner.Run(ctx, e.ws(b), "git", "clone", "--", b.SourceRemoteURL, "source-work"); err != nil {
			return fmt.Errorf("clone source work tree: %w", err)
		}
	}
	return nil
}

// ensureGiteaClone clones the Gitea mirror if it is not present yet.
func (e *Engine) ensureGiteaClone(ctx context.Context, b *Bridge) error {
	if exists(e.giteaClone(b)) {
		return nil
	}
	if _, err := e.Runner.Run(ctx, e.ws(b), "git", "clone", "--", b.GiteaSSHURL, "gitea-clone"); err != nil {
		return fmt.Errorf("clone gitea mirror: %w", err)
	}
	return nil
}

// CleanWorkspace makes a workspace safe to run a new job in after a crash
// (spec §7): abort stale `git am` state and drop leftover temp dirs.
func (e *Engine) CleanWorkspace(ctx context.Context, b *Bridge) {
	work := e.sourceWork(b)
	if exists(filepath.Join(work, ".git", "rebase-apply")) || exists(filepath.Join(work, ".git", "rebase-merge")) {
		e.Runner.Log("warning: stale am/rebase state found in source-work; aborting it")
		_, _ = e.Runner.Quiet(ctx, work, "git", "am", "--abort")
		_, _ = e.Runner.Quiet(ctx, work, "git", "rebase", "--abort")
	}
	entries, _ := os.ReadDir(e.ws(b))
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "tmp-") {
			_ = os.RemoveAll(filepath.Join(e.ws(b), ent.Name()))
		}
	}
}

// tempDir creates a temp dir inside the bridge workspace so CleanWorkspace
// can collect strays after a crash.
func (e *Engine) tempDir(b *Bridge, purpose string) (string, error) {
	return os.MkdirTemp(e.ws(b), "tmp-"+purpose+"-")
}

// CheckRefName validates a branch name with git itself (spec §9.5).
func (e *Engine) CheckRefName(ctx context.Context, name string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid branch name %q", name)
	}
	if _, err := e.Runner.Quiet(ctx, "", "git", "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("invalid branch name %q", name)
	}
	return nil
}
