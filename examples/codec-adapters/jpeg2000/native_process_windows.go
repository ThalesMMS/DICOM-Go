//go:build windows

package jpeg2000

import (
	"os/exec"
	"syscall"
	"time"
)

func configureNativeHelperCommand(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	c.WaitDelay = 2 * time.Second
}
func killNativeHelperProcess(c *exec.Cmd) error { return c.Process.Kill() }
