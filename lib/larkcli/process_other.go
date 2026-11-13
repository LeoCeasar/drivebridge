//go:build unix

package larkcli

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureCommand(*exec.Cmd) {}

func runBusinessCommand(cmd *exec.Cmd) (bool, error) {
	// The CLI may spawn helpers which inherit pipes or credentials. Keep them in
	// one process group and terminate that group before releasing credential locks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = stop
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return false, &Error{Type: "process", Subtype: "start"}
	}
	err := cmd.Wait()
	_ = stop()
	return true, err
}
