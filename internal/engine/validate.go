package engine

import (
	"fmt"
	"strings"
)

// ValidateExcludedPath enforces spec §5.1: plain relative directory paths
// only — no regex metacharacters (they are spliced into the guard pattern
// and the bash reference's grep alternation), no whitespace edges, no
// leading '/', no '..'.
func ValidateExcludedPath(p string) error {
	if p == "" {
		return fmt.Errorf("excluded path must not be empty")
	}
	if strings.TrimSpace(p) != p {
		return fmt.Errorf("excluded path %q has leading/trailing whitespace", p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("excluded path %q must be relative (no leading /)", p)
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("excluded path %q must not end with /", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("excluded path %q contains '.', '..' or empty segments", p)
		}
	}
	if i := strings.IndexAny(p, "\\^$.|?*+()[]{}"); i >= 0 {
		return fmt.Errorf("excluded path %q contains regex metacharacter %q", p, p[i])
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("excluded path %q contains a control character", p)
		}
	}
	return nil
}

// ValidateExcludedPaths validates the whole set.
func ValidateExcludedPaths(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("at least one excluded path is required")
	}
	for _, p := range paths {
		if err := ValidateExcludedPath(p); err != nil {
			return err
		}
	}
	return nil
}
