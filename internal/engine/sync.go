package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Sync implements spec §12.1: update the source mirror, run a deterministic
// git filter-repo over a fresh clone, store the commit-map, and force-push
// the configured branches/globs to Gitea.
//
// This is the only code path in Sluice that pushes to Gitea, and it only
// ever pushes the filter-repo output (spec §9.2).
func (e *Engine) Sync(ctx context.Context, b *Bridge) error {
	if err := ValidateExcludedPaths(b.ExcludedPaths); err != nil {
		return err
	}
	if !exists(e.sourceMirror(b)) {
		return fmt.Errorf("source mirror missing — run the init job first")
	}

	out, err := e.Runner.Run(ctx, e.sourceMirror(b), "git", "remote", "update", "--prune")
	if err != nil {
		return fmt.Errorf("update source mirror: %w", err)
	}
	if strings.Contains(out, "forced update") {
		e.Runner.Log("WARNING: source history was rewritten upstream (forced update). " +
			"Filtered SHAs for rewritten ranges will change; agent branches based on them may become orphaned.")
	}

	tmp, err := e.tempDir(b, "sync")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// --no-local gives filter-repo the fresh clone it requires and keeps the
	// mirror's object store untouched.
	if _, err := e.Runner.Run(ctx, "", "git", "clone", "--no-local", "--", e.sourceMirror(b), filepath.Join(tmp, "filtered")); err != nil {
		return fmt.Errorf("clone for filtering: %w", err)
	}
	filtered := filepath.Join(tmp, "filtered")

	args := []string{"filter-repo", "--invert-paths"}
	for _, p := range b.ExcludedPaths {
		args = append(args, "--path", p)
	}
	if _, err := e.Runner.Run(ctx, filtered, "git", args...); err != nil {
		return fmt.Errorf("git filter-repo: %w", err)
	}

	if err := copyFile(filepath.Join(filtered, ".git", "filter-repo", "commit-map"), e.commitMapPath(b)); err != nil {
		return fmt.Errorf("store commit-map: %w", err)
	}
	e.Runner.Log("commit-map stored at " + e.commitMapPath(b))

	if _, err := e.Runner.Run(ctx, filtered, "git", "remote", "add", "gitea", b.GiteaSSHURL); err != nil {
		return err
	}
	for _, br := range b.SyncBranches {
		if err := e.CheckRefName(ctx, br); err != nil {
			return err
		}
		ref := "refs/heads/" + br
		if _, err := e.Runner.Run(ctx, filtered, "git", "push", "--force", "gitea", ref+":"+ref); err != nil {
			return fmt.Errorf("push branch %s to gitea: %w", br, err)
		}
	}
	for _, g := range b.SyncGlobs {
		// Per the reference, glob pushes are best-effort (`|| true`): the
		// pattern may match nothing.
		if _, err := e.Runner.Run(ctx, filtered, "git", "push", "--force", "gitea", g+":"+g); err != nil {
			e.Runner.Log(fmt.Sprintf("note: glob push %q failed (non-fatal): %v", g, err))
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
