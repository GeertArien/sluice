package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ErrGuardViolation means a promotion patch touches an excluded path.
// Spec §9.1: hard fail, promotion rejected, no override.
type ErrGuardViolation struct {
	PatchFiles []string // offending patch file names (base names)
	Lines      []string // the matching diff header lines
}

func (e *ErrGuardViolation) Error() string {
	return fmt.Sprintf("SECURITY GUARD: patches touch excluded paths (%s)",
		strings.Join(e.PatchFiles, ", "))
}

// guardPattern builds the spec §12.2 step-4 expression with each excluded
// path regex-escaped (validation already rejects metacharacters; escaping
// is defense in depth) and path-boundary anchoring.
func guardPattern(excluded []string) (*regexp.Regexp, error) {
	if len(excluded) == 0 {
		return nil, fmt.Errorf("guard requires at least one excluded path")
	}
	quoted := make([]string, len(excluded))
	for i, p := range excluded {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return regexp.Compile(`^(\+\+\+ b|--- a)/(` + strings.Join(quoted, "|") + `)(/|$)`)
}

// GuardPatches scans the patch files emitted by format-patch and returns
// ErrGuardViolation if any `+++ b/<path>` or `--- a/<path>` header falls
// under an excluded path. It runs on the exact files that will be applied
// — not a recomputed diff (spec §9.1).
func GuardPatches(patchDir string, excluded []string) error {
	re, err := guardPattern(excluded)
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(patchDir, "*.patch"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	violation := &ErrGuardViolation{}
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		hit := false
		for sc.Scan() {
			line := sc.Text()
			if re.MatchString(line) {
				hit = true
				violation.Lines = append(violation.Lines, line)
			}
		}
		scanErr := sc.Err()
		f.Close()
		if scanErr != nil {
			return scanErr
		}
		if hit {
			violation.PatchFiles = append(violation.PatchFiles, filepath.Base(file))
		}
	}
	if len(violation.PatchFiles) > 0 {
		return violation
	}
	return nil
}
