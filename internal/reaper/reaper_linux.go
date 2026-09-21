//go:build linux

package reaper

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func start(ctx context.Context, interval time.Duration, logf func(string, ...any)) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	go func() {
		defer signal.Stop(sigs)
		t := time.NewTicker(interval)
		defer t.Stop()
		seen := map[int]struct{}{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigs:
			case <-t.C:
			}
			reapOnce(os.Getpid(), seen, logf)
		}
	}()
}

// zombie is an exited, unreaped process found in /proc.
type zombie struct {
	pid  int
	comm string
}

// reapOnce reaps every zombie child of self that is not held and was already
// a zombie at the previous scan. seen carries the sightings between scans.
func reapOnce(self int, seen map[int]struct{}, logf func(string, ...any)) {
	zs := zombieChildren(self)
	now := map[int]struct{}{}
	var reaped []string
	for _, z := range zs {
		if isHeld(z.pid) {
			continue
		}
		now[z.pid] = struct{}{}
		if _, ok := seen[z.pid]; !ok {
			continue // first sighting: give os/exec a chance to Wait on it
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(z.pid, &ws, syscall.WNOHANG, nil); err == nil {
			reaped = append(reaped, z.comm)
		}
		delete(now, z.pid)
	}
	for pid := range seen {
		delete(seen, pid)
	}
	for pid := range now {
		seen[pid] = struct{}{}
	}
	if len(reaped) > 0 && logf != nil {
		logf("reaper: reaped %d orphaned process(es): %s", len(reaped), summarize(reaped))
	}
}

// zombieChildren lists processes in /proc whose state is Z and whose parent
// is self. Exited-but-unreaped processes keep their /proc entry, which is
// why they can be found here even though they left their cgroup listing.
func zombieChildren(self int) []zombie {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []zombie
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// "<pid> (<comm>) <state> <ppid> ..." — comm may contain spaces and
		// parentheses, so split after the last ')'.
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) < 2 || fields[0] != "Z" {
			continue
		}
		if ppid, _ := strconv.Atoi(fields[1]); ppid != self {
			continue
		}
		comm := ""
		if j := strings.IndexByte(s, '('); j >= 0 && j < i {
			comm = s[j+1 : i]
		}
		out = append(out, zombie{pid: pid, comm: comm})
	}
	return out
}

func summarize(names []string) string {
	counts := map[string]int{}
	for _, n := range names {
		counts[n]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}
