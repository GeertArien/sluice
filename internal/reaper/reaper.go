// Package reaper collects orphaned zombie processes when Sluice runs as
// PID 1 (or as a child subreaper).
//
// A container's PID 1 inherits every orphaned process in the container and
// is expected to wait() on them when they exit; a plain Go program never
// does, so each orphan that finishes stays a zombie and keeps a slot in the
// container's pids cgroup until the limit is hit and fork() fails. The image
// ships tini as PID 1 so this normally never runs — it is the fallback for
// deployments that put the sluice binary itself at PID 1.
//
// Only zombies whose parent is this process and that are not currently
// owned by os/exec (see Hold) are reaped, and only after they have been
// seen as zombies in two consecutive scans, so a child that os/exec is about
// to Wait on is never stolen from it.
package reaper

import (
	"context"
	"sync"
	"time"
)

var (
	heldMu sync.Mutex
	held   = map[int]struct{}{}
)

// Hold marks pid as owned by the caller (typically an os/exec child that
// will be waited on) so the reaper skips it. The returned func releases it.
func Hold(pid int) func() {
	heldMu.Lock()
	held[pid] = struct{}{}
	heldMu.Unlock()
	return func() {
		heldMu.Lock()
		delete(held, pid)
		heldMu.Unlock()
	}
}

func isHeld(pid int) bool {
	heldMu.Lock()
	defer heldMu.Unlock()
	_, ok := held[pid]
	return ok
}

// Start runs the reaper in the background until ctx is done. It scans on
// SIGCHLD and every interval. logf (optional) receives one line per batch
// of reaped processes.
func Start(ctx context.Context, interval time.Duration, logf func(format string, args ...any)) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	start(ctx, interval, logf)
}
