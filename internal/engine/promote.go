package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrRejected is a clean, expected rejection (merge commits, empty branch,
// unresolvable base). The promotion is marked rejected with this reason.
type ErrRejected struct{ Reason string }

func (e *ErrRejected) Error() string { return "promotion rejected: " + e.Reason }

// ErrAmConflict means `git am --3way` stopped on a conflict. The workspace
// is intentionally left in the conflicted state so the operator can resolve
// it (spec §5.4); Recovery describes exactly how.
type ErrAmConflict struct {
	Workdir    string
	RealBranch string
}

func (e *ErrAmConflict) Error() string { return "git am conflict on " + e.RealBranch }

func (e *ErrAmConflict) Recovery() string {
	return fmt.Sprintf(`git am stopped on a conflict. To resolve manually:

    cd %s
    # fix the conflicted files, then:
    git add -A
    git am --continue        # repeat until all patches apply
    git push -u origin %s

then use "Mark promoted manually" with the tip SHA (git rev-parse HEAD).
Or abort from the UI, which runs: git am --abort && git checkout -`,
		e.Workdir, e.RealBranch)
}

type PromoteResult struct {
	RealBranch string
	TipSHA     string
	BaseReal   string
	NumCommits int
}

// CommitInfo is used by the promotion preview (spec §5.4).
type CommitInfo struct {
	SHA     string
	Author  string
	Subject string
	Files   []string
}

// Preflight holds the §5.4 pre-promotion checks for the UI.
type Preflight struct {
	TipSHA       string
	BaseFiltered string
	BaseReal     string
	BaseRealErr  string // commit-map resolution failure, if any
	MergeCount   int    // must be 0 (linearity)
	CommitCount  int    // 0 → empty branch
	Behind       int    // commits on mirror base not in branch (stale warning)
	GuardOK      bool
	GuardDetail  string
	Commits      []CommitInfo
}

// resolveBranch fetches the mirror clone and resolves tip + filtered/real base.
func (e *Engine) resolveBranch(ctx context.Context, b *Bridge, branch, base string) (tip, baseFiltered, baseReal string, err error) {
	if err = e.ensureGiteaClone(ctx, b); err != nil {
		return
	}
	clone := e.giteaClone(b)
	if _, err = e.Runner.Run(ctx, clone, "git", "fetch", "--prune", "origin"); err != nil {
		return
	}
	if err = e.CheckRefName(ctx, branch); err != nil {
		return
	}
	if err = e.CheckRefName(ctx, base); err != nil {
		return
	}
	out, err := e.Runner.Run(ctx, clone, "git", "rev-parse", "origin/"+branch)
	if err != nil {
		err = &ErrRejected{Reason: fmt.Sprintf("branch %q not found on the Gitea mirror", branch)}
		return
	}
	tip = strings.TrimSpace(out)
	out, err = e.Runner.Run(ctx, clone, "git", "merge-base", "origin/"+base, tip)
	if err != nil {
		err = &ErrRejected{Reason: fmt.Sprintf("no merge-base between origin/%s and %s", base, tip)}
		return
	}
	baseFiltered = strings.TrimSpace(out)
	baseReal, err = LookupRealSHA(e.commitMapPath(b), baseFiltered)
	return
}

func (e *Engine) countRange(ctx context.Context, dir, rangeExpr string, extra ...string) (int, error) {
	// The revision range must precede extra args: pathspec excludes arrive as
	// "-- . :(exclude)…", and anything after "--" is treated as a path, so a
	// range placed after it is misparsed (git exits 129). Range first keeps
	// both flag extras (e.g. --merges) and pathspec extras valid.
	args := append([]string{"rev-list", "--count", rangeExpr}, extra...)
	out, err := e.Runner.Quiet(ctx, dir, "git", args...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// RunPreflight computes the §5.4 checks without touching source-work.
func (e *Engine) RunPreflight(ctx context.Context, b *Bridge, branch, base string) (*Preflight, error) {
	pf := &Preflight{}
	tip, baseFiltered, baseReal, err := e.resolveBranch(ctx, b, branch, base)
	if err != nil {
		var rej *ErrRejected
		if errors.As(err, &rej) {
			return nil, err
		}
		var unmapped *ErrUnmappedCommit
		if errors.As(err, &unmapped) || strings.Contains(err.Error(), "commit-map") {
			pf.BaseRealErr = err.Error()
		} else {
			return nil, err
		}
	}
	pf.TipSHA, pf.BaseFiltered, pf.BaseReal = tip, baseFiltered, baseReal
	clone := e.giteaClone(b)
	ignore := b.ignorePathspec()

	if pf.MergeCount, err = e.countRange(ctx, clone, baseFiltered+".."+tip, "--merges"); err != nil {
		return nil, err
	}
	// CommitCount reflects what will actually be promoted: commits that still
	// have changes after the promotion-ignored paths are excluded.
	if pf.CommitCount, err = e.countRange(ctx, clone, baseFiltered+".."+tip, ignore...); err != nil {
		return nil, err
	}
	if pf.Behind, err = e.countRange(ctx, clone, tip+"..origin/"+base); err != nil {
		return nil, err
	}

	// Commit list for the preview (also excluding ignored paths).
	logArgs := append([]string{"log", "--reverse", "--format=%H%x1f%an <%ae>%x1f%s", baseFiltered + ".." + tip}, ignore...)
	out, err := e.Runner.Quiet(ctx, clone, "git", logArgs...)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) != 3 {
			continue
		}
		ci := CommitInfo{SHA: parts[0], Author: parts[1], Subject: parts[2]}
		showArgs := append([]string{"show", "--name-only", "--format=", parts[0]}, ignore...)
		files, _ := e.Runner.Quiet(ctx, clone, "git", showArgs...)
		for _, f := range strings.Split(strings.TrimSpace(files), "\n") {
			if f != "" {
				ci.Files = append(ci.Files, f)
			}
		}
		pf.Commits = append(pf.Commits, ci)
	}

	// Guard check on real format-patch output (same files promotion uses).
	patchDir, err := e.tempDir(b, "preflight")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(patchDir)
	fpArgs := append([]string{"format-patch", "--binary", "-o", patchDir, baseFiltered + ".." + tip}, ignore...)
	if _, err := e.Runner.Quiet(ctx, clone, "git", fpArgs...); err != nil {
		return nil, err
	}
	if gerr := GuardPatches(patchDir, b.ExcludedPaths); gerr != nil {
		var gv *ErrGuardViolation
		if errors.As(gerr, &gv) {
			pf.GuardOK = false
			pf.GuardDetail = strings.Join(gv.Lines, "\n")
		} else {
			return nil, gerr
		}
	} else {
		pf.GuardOK = true
	}
	return pf, nil
}

// Promote implements spec §12.2. target is the branch name to create on the
// source remote; when empty it defaults to the agent branch name. On guard
// violation it returns *ErrGuardViolation; on an am conflict it returns
// *ErrAmConflict and leaves source-work mid-am for manual resolution.
func (e *Engine) Promote(ctx context.Context, b *Bridge, branch, base, target string) (*PromoteResult, error) {
	if target == "" {
		target = branch
	}
	if err := e.CheckRefName(ctx, target); err != nil {
		return nil, &ErrRejected{Reason: fmt.Sprintf("invalid target branch name %q", target)}
	}
	if target == base {
		return nil, &ErrRejected{Reason: fmt.Sprintf("target branch must differ from the base branch %q", base)}
	}
	// 1. resolve
	tip, baseFiltered, baseReal, err := e.resolveBranch(ctx, b, branch, base)
	if err != nil {
		return nil, err
	}
	e.Runner.Log(fmt.Sprintf("tip=%s base(filtered)=%s base(real)=%s", tip, baseFiltered, baseReal))
	clone := e.giteaClone(b)

	// 2. linearity
	merges, err := e.countRange(ctx, clone, baseFiltered+".."+tip, "--merges")
	if err != nil {
		return nil, err
	}
	if merges > 0 {
		return nil, &ErrRejected{Reason: fmt.Sprintf(
			"branch contains %d merge commit(s); ask the agent to rebase onto %s, force-push, then retry", merges, base)}
	}
	count, err := e.countRange(ctx, clone, baseFiltered+".."+tip)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, &ErrRejected{Reason: "branch is even with " + base + " — nothing to promote"}
	}

	// 3. export (excluding promotion-ignored paths, e.g. mirror-only build helpers)
	patchDir, err := e.tempDir(b, "patches")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(patchDir)
	fpArgs := append([]string{"format-patch", "--binary", "-o", patchDir, baseFiltered + ".." + tip}, b.ignorePathspec()...)
	if _, err := e.Runner.Run(ctx, clone, "git", fpArgs...); err != nil {
		return nil, fmt.Errorf("format-patch: %w", err)
	}
	if patches, _ := filepath.Glob(filepath.Join(patchDir, "*.patch")); len(patches) == 0 {
		return nil, &ErrRejected{Reason: "nothing to promote — all changes are under promotion-ignored paths"}
	}

	// 4. SECURITY GUARD (spec §9.1: hard fail, no override)
	if err := GuardPatches(patchDir, b.ExcludedPaths); err != nil {
		return nil, err
	}
	e.Runner.Log("security guard: PASS (no excluded paths touched)")

	// 5. apply onto real history
	work := e.sourceWork(b)
	realBranch := target
	if _, err := e.Runner.Run(ctx, work, "git", "fetch", "origin"); err != nil {
		return nil, err
	}
	// Make the Gitea mirror's objects available so `git am --3way` can rebuild
	// the base tree for patches whose recorded blobs live only on the mirror.
	// Best-effort: failing only forfeits the 3-way fallback, so don't abort.
	if err := e.fetchMirrorObjects(ctx, b); err != nil {
		e.Runner.Log("warning: could not fetch mirror objects for 3-way merge: " + err.Error())
	}
	if _, err := e.Runner.Run(ctx, work, "git", "checkout", "-B", realBranch, baseReal); err != nil {
		return nil, fmt.Errorf("checkout %s at %s: %w", realBranch, baseReal, err)
	}
	patches, err := filepath.Glob(filepath.Join(patchDir, "*.patch"))
	if err != nil {
		return nil, err
	}
	sort.Strings(patches)
	committerName, committerEmail := b.PromoteName, b.PromoteEmail
	if committerName == "" {
		committerName, committerEmail = "Sluice", "sluice@localhost"
	}
	// --keep-cr: am splits patches via mailsplit, which strips CR from line
	// ends by default. For files committed with CRLF endings (e.g. Windows
	// project files) that corrupts the patch so it applies to nothing — not
	// even its own recorded pre-image blob ("does not apply to blobs recorded
	// in its index"). Our patches always come from format-patch, never real
	// e-mail, so keeping CR is always byte-correct.
	//
	// --keep-non-patch: mailinfo strips ALL leading [..] groups from the
	// subject by default, which eats issue tags like "[SIMU-1736] ...".
	// -b/--keep-non-patch limits stripping to bracket groups containing the
	// word PATCH, so it removes the "[PATCH]" that format-patch adds while
	// preserving the author's real subject and any [TICKET] prefix.
	amArgs := []string{"-c", "user.name=" + committerName, "-c", "user.email=" + committerEmail,
		"am", "--3way", "--keep-cr", "--keep-non-patch"}
	if b.PromoteSignoff {
		amArgs = append(amArgs, "--signoff")
	}
	amArgs = append(amArgs, patches...)
	if _, err := e.Runner.Run(ctx, work, "git", amArgs...); err != nil {
		return nil, &ErrAmConflict{Workdir: work, RealBranch: realBranch}
	}

	// 6. optional identity rewrite (author AND committer; keeps author dates)
	if b.PromoteName != "" {
		if err := e.rewriteIdentity(ctx, b, baseReal); err != nil {
			return nil, fmt.Errorf("identity rewrite: %w", err)
		}
	}

	// 7. push + record
	out, err := e.Runner.Run(ctx, work, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	tipReal := strings.TrimSpace(out)
	if _, err := e.Runner.Run(ctx, work, "git", "push", "-u", "origin", realBranch); err != nil {
		return nil, fmt.Errorf("push %s to source remote: %w", realBranch, err)
	}
	return &PromoteResult{RealBranch: realBranch, TipSHA: tipReal, BaseReal: baseReal, NumCommits: count}, nil
}

// rewriteIdentitySh is the fixed §12.2 step-6 script. All variable input
// arrives via the environment — nothing is interpolated into shell text.
const rewriteIdentitySh = `#!/bin/sh
set -e
orig_name=$(git log -1 --format='%an')
orig_email=$(git log -1 --format='%ae')
orig_date=$(git log -1 --format='%aD')
set -- --amend --no-edit --author="$SLUICE_PROMOTE_NAME <$SLUICE_PROMOTE_EMAIL>" --date="$orig_date"
if [ "$SLUICE_PROMOTE_KEEP_TRAILER" = "true" ] && [ "$orig_email" != "$SLUICE_PROMOTE_EMAIL" ]; then
  set -- "$@" --trailer "Co-authored-by: $orig_name <$orig_email>"
fi
git -c user.name="$SLUICE_PROMOTE_NAME" -c user.email="$SLUICE_PROMOTE_EMAIL" commit "$@"
`

func (e *Engine) rewriteIdentity(ctx context.Context, b *Bridge, baseReal string) error {
	script := filepath.Join(e.ws(b), "rewrite-identity.sh")
	if err := os.WriteFile(script, []byte(rewriteIdentitySh), 0o755); err != nil {
		return err
	}
	env := []string{
		"SLUICE_PROMOTE_NAME=" + b.PromoteName,
		"SLUICE_PROMOTE_EMAIL=" + b.PromoteEmail,
		"SLUICE_PROMOTE_KEEP_TRAILER=" + strconv.FormatBool(b.PromoteKeepTrailer),
		"SLUICE_REWRITE_SCRIPT=" + script,
	}
	// The --exec string is a fixed literal; the script path travels via the
	// environment so untrusted values never enter shell syntax.
	_, err := e.Runner.RunEnv(ctx, e.sourceWork(b), env, "git",
		"-c", "user.name="+b.PromoteName, "-c", "user.email="+b.PromoteEmail,
		"rebase", "--force-rebase", "--exec", `sh "$SLUICE_REWRITE_SCRIPT"`, baseReal)
	if err != nil {
		_, _ = e.Runner.Quiet(ctx, e.sourceWork(b), "git", "rebase", "--abort")
		return err
	}
	return nil
}

// AbortPromotion implements the one-click abort for a conflicted promotion.
func (e *Engine) AbortPromotion(ctx context.Context, b *Bridge) error {
	work := e.sourceWork(b)
	_, _ = e.Runner.Quiet(ctx, work, "git", "am", "--abort")
	_, _ = e.Runner.Quiet(ctx, work, "git", "rebase", "--abort")
	if _, err := e.Runner.Run(ctx, work, "git", "reset", "--hard"); err != nil {
		return err
	}
	return nil
}
