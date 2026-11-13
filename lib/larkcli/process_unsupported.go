//go:build !unix && !windows

package larkcli

import "os/exec"

func configureCommand(*exec.Cmd) {}

// Business credentials may only be dispatched when helper processes can be
// contained through cancellation and cleanup.
func runBusinessCommand(*exec.Cmd) (bool, error) {
	return false, &Error{Type: "process", Subtype: "containment"}
}
