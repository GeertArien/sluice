//go:build !unix

package execx

import "os/exec"

// setProcessGroup is a no-op where process groups are unavailable; the
// context still kills the direct child.
func setProcessGroup(cmd *exec.Cmd) {}
