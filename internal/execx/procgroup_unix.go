//go:build unix

package execx

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and makes
// cancellation kill that whole group. git spawns helpers (ssh, upload-pack,
// pack-objects, gc) that would otherwise survive a kill of git itself and end
// up orphaned under PID 1.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// With Setpgid the group id equals the child's pid.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
}
