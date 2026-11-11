package platform

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

type StartupSpec struct {
	ID, Executable string
	Args           []string
}

func validateStartup(s StartupSpec) error {
	id, err := uuid.Parse(s.ID)
	if err != nil || id == uuid.Nil || id.String() != s.ID || !filepath.IsAbs(s.Executable) {
		return exec.ErrNotFound
	}
	for _, arg := range append([]string{s.Executable}, s.Args...) {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return exec.ErrNotFound
		}
	}
	return nil
}
func runStartup(ctx context.Context, name string, args ...string) error {
	cmd := BackgroundCommand(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}
