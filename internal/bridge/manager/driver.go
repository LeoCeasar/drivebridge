package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
)

func ResolveCLI(explicit, bridgeExecutable string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		base := filepath.Dir(bridgeExecutable)
		if runtime.GOOS == "windows" {
			candidates = append(candidates, filepath.Join(base, "tools", "lark-cli", "node_modules", "@larksuite", "cli", "bin", "lark-cli.exe"), filepath.Join(base, "tools", "lark-cli", "lark-cli.exe"))
		} else {
			candidates = append(candidates, filepath.Join(base, "tools", "lark-cli", "lark-cli"), filepath.Join(base, "lark-cli"))
		}
		candidates = append(candidates, "lark-cli")
	}
	for _, p := range candidates {
		if !filepath.IsAbs(p) {
			var err error
			p, err = exec.LookPath(p)
			if err != nil {
				continue
			}
		}
		p, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		p, err = filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		info, err := os.Stat(p)
		if err == nil && info.Mode().IsRegular() {
			if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
				continue
			}
			return p, nil
		}
	}
	return "", ErrDriver
}
func trustedGrants(path string) ([]cloud.CapabilityGrant, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return nil, "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || sum != "a8f652c2a62577a152467a983ec60548570fb0a08715e95fa3f579438c81885b" {
		return nil, sum, nil
	}
	out := []cloud.CapabilityGrant{}
	for _, kind := range []string{model.OperationBlockReplace, model.OperationBlockInsertAfter, model.OperationBlockDelete} {
		out = append(out, cloud.CapabilityGrant{Descriptor: model.OperationCapability{ID: "feishu-cli-1.0.99-" + kind, Kind: kind, Route: cloud.ShortcutXMLRoute}, CLISourceRevision: cloud.CLIRevision, Evidence: "live:2026-11-10-scoped-docx-subset-and-prepend0;sha256:" + sum, Verified: true})
	}
	return out, sum, nil
}
func isDrive(path string) bool {
	return runtime.GOOS == "windows" && len(path) >= 2 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (len(path) == 2 || path[2:] == `\` || path[2:] == "/")
}
