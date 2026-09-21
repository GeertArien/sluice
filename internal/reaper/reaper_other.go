//go:build !linux

package reaper

import (
	"context"
	"time"
)

// start is a no-op outside Linux: Sluice's container image is Linux-only and
// the orphan-inheritance problem is specific to running as PID 1 there.
func start(ctx context.Context, interval time.Duration, logf func(string, ...any)) {}
