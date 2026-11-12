// Sync files and directories to and from local and remote object stores
//
// Nick Craig-Wood <nick@craig-wood.com>
package main

import (
	_ "github.com/rclone/rclone/backend/all" // import all backends
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all"    // import all commands
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == "drivebridge-desktop" && len(os.Args) == 1 {
		os.Args = append(os.Args, "desktop", "_run")
	}
	if strings.HasPrefix(strings.ToLower(filepath.Base(os.Args[0])), "drivebridge") {
		cmd.Root.Use = "drivebridge"
	}
	cmd.Main()
}
