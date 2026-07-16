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

	PromoteName        string
	PromoteEmail       string
	PromoteKeepTrailer bool
	PromoteSignoff     bool
	// PromoteIgnorePaths are dropped from the patches a promotion applies to
	// the source (e.g. mirror-only build helpers). They stay on the mirror.
	PromoteIgnorePaths []string
}

// ignorePathspec turns the promotion-ignored paths into a git pathspec that
// keeps everything except those paths (`-- . :(exclude)<p>…`); nil when the
// list is empty so existing behavior is unchanged.
func (b *Bridge) ignorePathspec() []string {
	if len(b.PromoteIgnorePaths) == 0 {
		return nil
	}
	args := []string{"--", "."}
	for _, p := range b.PromoteIgnorePaths {
		args = append(args, ":(exclude)"+p)
	}
	return args
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

// fetchMirrorObjects copies the Gitea mirror's objects into source-work so that
// `git am --3way` can reconstruct the base tree for patches whose recorded blobs
// exist only on the mirror. source-work is a clone of the SOURCE remote and
// never received the mirror's objects, so without this the 3-way fallback fails
// with "could not build fake ancestor" / "does not apply to blobs recorded in
// its index" on any patch that needs it. The mirror's refs land under a
// dedicated refs/gitea-mirror/* namespace and are never pushed upstream.
func (e *Engine) fetchMirrorObjects(ctx context.Context, b *Bridge) error {
	_, err := e.Runner.Run(ctx, e.sourceWork(b), "git", "fetch", "--no-tags",
		e.giteaClone(b), "+refs/remotes/origin/*:refs/gitea-mirror/*")
	return err
}

// CleanWorkspace makes a workspace safe to run a new job in after a crash
// (spec §7): abort stale `git am` state and drop leftover temp dirs.
func (e *Engine) CleanWorkspace(ctx context.Context, b *Bridge) {
	work := e.sourceWork(b)
	rebaseApply := filepath.Join(work, ".git", "rebase-apply")
	rebaseMerge := filepath.Join(work, ".git", "rebase-merge")
	if exists(rebaseApply) || exists(rebaseMerge) {
		e.Runner.Log("warning: stale am/rebase state found in source-work; aborting it")
		_, _ = e.Runner.Quiet(ctx, work, "git", "am", "--abort")
		_, _ = e.Runner.Quiet(ctx, work, "git", "rebase", "--abort")
		// A wedged `git am` can leave rebase-apply behind (or the abort itself
		// can fail), and then every future `git am` dies with "previous rebase
		// directory .git/rebase-apply still exists but mbox given". Force-remove
		// it and reset the tree so it can never permanently block the bridge.
		if exists(rebaseApply) || exists(rebaseMerge) {
			e.Runner.Log("warning: am/rebase state persisted after abort; force-clearing it")
			_ = os.RemoveAll(rebaseApply)
			_ = os.RemoveAll(rebaseMerge)
			_, _ = e.Runner.Quiet(ctx, work, "git", "reset", "--hard")
		}
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
