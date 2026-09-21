package execx

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestWithGitConfig(t *testing.T) {
	kv := [][2]string{{"a.b", "1"}, {"c.d", "2"}}

	got := withGitConfig([]string{"PATH=/bin"}, kv)
	want := []string{"PATH=/bin",
		"GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1",
		"GIT_CONFIG_KEY_1=c.d", "GIT_CONFIG_VALUE_1=2",
		"GIT_CONFIG_COUNT=2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fresh env:\n%s", strings.Join(got, "\n"))
	}

	// An existing sequence is continued, not clobbered.
	got = withGitConfig([]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=x.y", "GIT_CONFIG_VALUE_0=z"}, kv[:1])
	want = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=x.y", "GIT_CONFIG_VALUE_0=z",
		"GIT_CONFIG_KEY_1=a.b", "GIT_CONFIG_VALUE_1=1",
		"GIT_CONFIG_COUNT=2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("continued env:\n%s", strings.Join(got, "\n"))
	}
}

// Every git run through the Runner must see auto maintenance forced into the
// foreground, also when the caller injects its own GIT_CONFIG_* entries.
func TestRunForcesForegroundGitMaintenance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	r := &Runner{Env: []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=sluice.test", "GIT_CONFIG_VALUE_0=yes",
	}}
	ctx := context.Background()
	if _, err := r.Run(ctx, dir, "git", "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"gc.autoDetach":          "false",
		"maintenance.autoDetach": "false",
		"sluice.test":            "yes",
	} {
		out, err := r.Run(ctx, dir, "git", "config", "--get", key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
