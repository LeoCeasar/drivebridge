//go:build !windows

package ownedlogin

import "os/exec"

func configureHelperProcess(cmd *exec.Cmd) {}
