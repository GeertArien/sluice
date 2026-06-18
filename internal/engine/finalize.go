package engine

import (
	"context"
	"fmt"
	"strings"
)

// DetectLanded implements spec §12.3: a promotion has landed upstream if
// its real tip is an ancestor of origin/<base> (merge or fast-forward), or
// if `git cherry` reports no '+' lines (rebase-merge, patch-id
// equivalence). Squash merges are not detectable — manual finalize only.
func (e *Engine) DetectLanded(ctx context.Context, b *Bridge, realTipSHA, base string) (bool, error) {
	work := e.sourceWork(b)
	if _, err := e.Runner.Run(ctx, work, "git", "fetch", "--prune", "origin"); err != nil {
		return false, err
	}
	if _, err := e.Runner.Quiet(ctx, work, "git", "cat-file", "-e", realTipSHA+"^{commit}"); err != nil {
		return false, fmt.Errorf("recorded tip %s not found in source-work — was the workspace recreated?", realTipSHA)
	}
	if _, err := e.Runner.Quiet(ctx, work, "git", "merge-base", "--is-ancestor", realTipSHA, "origin/"+base); err == nil {
		return true, nil
	}
	out, err := e.Runner.Quiet(ctx, work, "git", "cherry", "origin/"+base, realTipSHA)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") {
			return false, nil
		}
	}
	return true, nil
}

// DeleteUpstreamBranch removes the promoted branch from the source remote
// after the change has landed (spec §12.3 cleanup). As a safety net it only
// deletes branches under the bridge's configured promotion prefix, so it can
// never delete an arbitrary upstream branch.
func (e *Engine) DeleteUpstreamBranch(ctx context.Context, b *Bridge, realBranch string) error {
	prefix := b.PromoteBranchPrefix
	if prefix == "" {
		prefix = "ai/"
	}
	if !strings.HasPrefix(realBranch, prefix) {
		return fmt.Errorf("refusing to delete %q upstream: not under promotion prefix %q", realBranch, prefix)
	}
	_, err := e.Runner.Run(ctx, e.sourceWork(b), "git", "push", "origin", "--delete", realBranch)
	return err
}
