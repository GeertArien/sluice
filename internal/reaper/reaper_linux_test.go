//go:build linux

package reaper

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const prSetChildSubreaper = 36

// Become a subreaper so orphans of our children land on us, exactly as they
// land on PID 1 in a container.
func becomeSubreaper(t *testing.T) {
	t.Helper()
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		t.Skipf("prctl(PR_SET_CHILD_SUBREAPER): %v", e)
	}
	t.Cleanup(func() { syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0) })
}

// orphan spawns a short-lived process whose parent exits immediately, the
// way git's detached `gc --auto` does, and returns its pid.
func orphan(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("sh", "-c", "sleep 0.1 & echo $!").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("pid %q: %v", out, err)
	}
	return pid
}

func procState(pid int) (string, bool) {
	st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	i := strings.LastIndexByte(string(st), ')')
	f := strings.Fields(string(st)[i+1:])
	return f[0], true
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, ok := procState(pid); !ok {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestReapsOrphanedZombies(t *testing.T) {
	becomeSubreaper(t)

	// Without the reaper the orphan stays a zombie under us.
	pid := orphan(t)
	time.Sleep(300 * time.Millisecond)
	if s, ok := procState(pid); !ok || s != "Z" {
		t.Fatalf("expected orphan %d to be a zombie under this process, state=%q ok=%v", pid, s, ok)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logged []string
	Start(ctx, 50*time.Millisecond, func(f string, a ...any) { logged = append(logged, f) })
	if !waitGone(pid, 3*time.Second) {
		t.Fatalf("orphan %d was not reaped", pid)
	}
	if len(logged) == 0 {
		t.Error("expected the reaper to log the batch")
	}

	// New orphans that appear while it runs are reaped too.
	if pid2 := orphan(t); !waitGone(pid2, 3*time.Second) {
		t.Fatalf("later orphan %d was not reaped", pid2)
	}
}

// A zombie held by os/exec (Hold) is left for cmd.Wait, even across scans.
func TestHeldChildIsNotStolen(t *testing.T) {
	becomeSubreaper(t)
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	release := Hold(cmd.Process.Pid)
	seen := map[int]struct{}{}
	self := os.Getpid()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s, _ := procState(cmd.Process.Pid); s == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		reapOnce(self, seen, nil)
	}
	if s, ok := procState(cmd.Process.Pid); !ok || s != "Z" {
		t.Fatalf("held child was reaped behind os/exec's back (state=%q ok=%v)", s, ok)
	}
	release()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait after reaper scans: %v", err)
	}
}
