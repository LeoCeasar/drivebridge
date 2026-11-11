//go:build !windows

package larkcli

import "os/exec"

func configureCommand(*exec.Cmd) {}
