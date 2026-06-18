// Package execx runs external commands for Sluice. All invocations are
// argv arrays (never a shell string), run with a timeout, and have their
// combined output captured through a scrubber that removes secret material
// before it can reach a job log.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds a single git/filter-repo invocation. First syncs of
// large repos can be slow, so this is generous; callers can override.
const DefaultTimeout = 30 * time.Minute

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
	if len(r.Env) > 0 {
		cmd.Env = append(cmd.Environ(), r.Env...)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	r.logf("$ %s %s", name, strings.Join(args, " "))
	err := cmd.Run()
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
