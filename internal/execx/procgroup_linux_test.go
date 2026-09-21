//go:build linux

package execx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A timed-out command must take its helpers with it: the grandchild here
// stands in for the ssh/upload-pack/gc processes git spawns.
func TestRunKillsWholeProcessGroupOnTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	r := &Runner{Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), "", "sh", "-c",
		"sleep 60 & echo $! > "+pidFile+"; wait")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	// Wait must not hang on the pipe the grandchild inherited.
	if el := time.Since(start); el > waitDelay {
		t.Fatalf("Run took %s, grandchild kept the pipes open", el)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("bad pid file %q", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return // gone
		}
		if i := strings.LastIndexByte(string(st), ')'); i >= 0 {
			if f := strings.Fields(string(st)[i+1:]); len(f) > 0 && f[0] == "Z" {
				return // dead, merely not yet reaped by whoever inherited it
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the timeout: %s", pid, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
