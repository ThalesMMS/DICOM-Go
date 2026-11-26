//go:build !windows

package jpeg2000

import (
	"os/exec"
	"syscall"
	"time"
)

func configureNativeHelperCommand(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.WaitDelay = 2 * time.Second
}
func killNativeHelperProcess(c *exec.Cmd) error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
