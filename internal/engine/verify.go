package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VerifyResult is the §5.2 leak-check report.
type VerifyResult struct {
	OK               bool     // false if any excluded path or tripwire hit
	PathFindings     []string // excluded paths still present in history
	TripwireFindings []string // tripwire strings found in blobs
	GitleaksRan      bool
	GitleaksFindings string // advisory only, never flips OK
}

// Verify clones the filtered repo from Gitea into a temp dir and checks that
// no excluded path appears anywhere in history and no tripwire string
// appears in any blob. gitleaks runs if present, as advisory output.
func (e *Engine) Verify(ctx context.Context, b *Bridge) (*VerifyResult, error) {
	tmp, err := e.tempDir(b, "verify")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	checkout := filepath.Join(tmp, "check")
	if _, err := e.Runner.Run(ctx, "", "git", "clone", "--", b.GiteaSSHURL, checkout); err != nil {
		return nil, fmt.Errorf("clone filtered repo for verification: %w", err)
	}
	res := &VerifyResult{OK: true}

	// 1. git log --all -- <path> must be empty for every excluded path.
	for _, p := range b.ExcludedPaths {
		out, err := e.Runner.Quiet(ctx, checkout, "git", "log", "--oneline", "--all", "--", p)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(out) != "" {
			res.OK = false
			res.PathFindings = append(res.PathFindings, p)
			e.Runner.Log(fmt.Sprintf("LEAK: history still references excluded path %q:\n%s", p, out))
		}
	}

	// 2. tripwire strings grepped across all blobs of all revisions.
	if len(b.TripwireStrings) > 0 {
		revs, err := e.Runner.Quiet(ctx, checkout, "git", "rev-list", "--all")
		if err != nil {
			return nil, err
		}
		revList := strings.Fields(revs)
		for _, s := range b.TripwireStrings {
			if s == "" {
				continue
			}
			if e.grepRevs(ctx, checkout, s, revList) {
				res.OK = false
				res.TripwireFindings = append(res.TripwireFindings, s)
				e.Runner.Log(fmt.Sprintf("LEAK: tripwire string %q found in filtered history", s))
			}
		}
	}

	// 3. gitleaks, advisory, only if the binary is present.
	if _, err := exec.LookPath("gitleaks"); err == nil {
		res.GitleaksRan = true
		out, gerr := e.Runner.Run(ctx, checkout, "gitleaks", "detect", "--source", ".", "--no-banner", "--exit-code", "0", "-v")
		res.GitleaksFindings = out
		if gerr != nil {
			e.Runner.Log("gitleaks run failed (advisory): " + gerr.Error())
		}
	} else {
		e.Runner.Log("gitleaks not installed; skipping secret scan (advisory)")
	}

	if res.OK {
		e.Runner.Log("verification PASSED: no excluded paths or tripwire strings in filtered history")
	}
	return res, nil
}

// grepRevs runs `git grep -I -F <s> <revs...>` in batches to stay under
// argv limits; returns true if the string appears in any blob.
func (e *Engine) grepRevs(ctx context.Context, dir, needle string, revs []string) bool {
	const batch = 200
	for i := 0; i < len(revs); i += batch {
		end := min(i+batch, len(revs))
		// -e protects needles that start with '-'; revs follow the pattern.
		args := append([]string{"grep", "-I", "-F", "-l", "-e", needle}, revs[i:end]...)
		if out, err := e.Runner.Quiet(ctx, dir, "git", args...); err == nil && strings.TrimSpace(out) != "" {
			return true
		}
	}
	return false
}
