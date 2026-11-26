//go:build windows

package netflix

import "os/exec"

// closeWindow ends the browser (Windows has no polite signal for it).
func closeWindow(c *exec.Cmd) { c.Process.Kill() }
