//go:build !windows

package netflix

import (
	"os/exec"
	"syscall"
)

// closeWindow asks the browser to quit, as closing its window would.
func closeWindow(c *exec.Cmd) { c.Process.Signal(syscall.SIGTERM) }
