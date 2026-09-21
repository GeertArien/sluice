// Package execx runs external commands for Sluice. All invocations are
// argv arrays (never a shell string), run with a timeout, and have their
// combined output captured through a scrubber that removes secret material
// before it can reach a job log.
//
// Two process-hygiene rules live here because every git invocation goes
// through this package:
//
//   - Each command runs in its own process group and the whole group is
//     killed on timeout/cancel, so a killed git cannot leave ssh,
//     upload-pack or gc helpers behind as orphans.
//   - git's automatic maintenance is forced to run in the foreground
//     (gc.autoDetach / maintenance.autoDetach = false). By default git forks
//     `gc --auto` into the background and lets the parent exit, which
//     reparents the gc process to PID 1. Inside a container where Sluice is
//     PID 1 nobody waits on it, and every such gc stays a zombie forever —
//     each holding a slot against the container's pids limit until fork()
//     fails with EAGAIN.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/geertarien/sluice/internal/reaper"
)

// DefaultTimeout bounds a single git/filter-repo invocation. First syncs of
// large repos can be slow, so this is generous; callers can override.
const DefaultTimeout = 30 * time.Minute

// waitDelay is how long Wait keeps waiting for the output pipes to close
// after the process group has been killed. It only matters if some helper
// escaped the group (e.g. by calling setsid) while still holding our pipes.
const waitDelay = 5 * time.Second

// gitConfig is injected into the environment of every command (git reads
// GIT_CONFIG_COUNT/KEY_n/VALUE_n since 2.31; other programs ignore it, and
// git-filter-repo's git subprocesses inherit it).
var gitConfig = [][2]string{
	{"gc.autoDetach", "false"},
	// git >= 2.47 routes auto maintenance through `git maintenance run --auto`,
	// whose detach setting falls back to gc.autoDetach; set it explicitly too.
	{"maintenance.autoDetach", "false"},
}

// Runner executes commands with a fixed environment and log sink.
type Runner struct {
	// Env is appended to the inherited environment (e.g. GIT_SSH_COMMAND).
	Env []string
	// Secrets are strings that must never appear in captured output.
	Secrets []string
	// Log receives human-readable progress lines (already scrubbed).
	Log func(line string)
	// Timeout per command; zero means DefaultTimeout.
	Timeout time.Duration
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(r.Scrub(fmt.Sprintf(format, args...)))
	}
}

// Scrub replaces all registered secrets in s with a placeholder.
func (r *Runner) Scrub(s string) string {
	for _, sec := range r.Secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[REDACTED]")
		}
	}
	// Belt and braces: mask anything that looks like an Authorization header.
	if i := strings.Index(s, "Authorization:"); i >= 0 {
		end := strings.IndexByte(s[i:], '\n')
		if end < 0 {
			end = len(s) - i
		}
		s = s[:i] + "Authorization: [REDACTED]" + s[i+end:]
	}
	return s
}

// Run executes name with args in dir, streaming combined output to the log.
// It returns the captured (scrubbed) output and an error if the command
// failed or timed out.
func (r *Runner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	cmd.Env = withGitConfig(append(cmd.Environ(), r.Env...), gitConfig)
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	r.logf("$ %s %s", name, strings.Join(args, " "))
	err := cmd.Start()
	if err == nil {
		// While we own this child, the orphan reaper must leave it alone:
		// cmd.Wait is the one that reaps it.
		release := reaper.Hold(cmd.Process.Pid)
		err = cmd.Wait()
		release()
	}
	out := r.Scrub(buf.String())
	if out != "" {
		r.logf("%s", strings.TrimRight(out, "\n"))
	}
	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// withGitConfig appends key/value pairs as GIT_CONFIG_* entries, continuing
// any GIT_CONFIG_COUNT sequence already present in env (last value wins, as
// with os/exec's duplicate-key handling).
func withGitConfig(env []string, kv [][2]string) []string {
	n := 0
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c >= 0 {
				n = c
			}
		}
	}
	for _, p := range kv {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, p[1]))
		n++
	}
	return append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
}

// RunEnv is Run with extra per-call environment variables (used to pass
// values into scripts without shell interpolation).
func (r *Runner) RunEnv(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	saved := r.Env
	r.Env = append(append([]string{}, saved...), extraEnv...)
	defer func() { r.Env = saved }()
	return r.Run(ctx, dir, name, args...)
}

// Quiet runs a command without logging the invocation line (still scrubbed);
// for probes whose failure is expected and meaningful.
func (r *Runner) Quiet(ctx context.Context, dir string, name string, args ...string) (string, error) {
	savedLog := r.Log
	r.Log = nil
	defer func() { r.Log = savedLog }()
	return r.Run(ctx, dir, name, args...)
}
