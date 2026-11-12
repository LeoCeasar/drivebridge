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
	if actual, err := filepath.EvalSymlinks(bridgeExecutable); err == nil {
		bridgeExecutable = actual
	}
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		base := filepath.Dir(bridgeExecutable)
		native := "lark-cli"
		if runtime.GOOS == "windows" {
			native += ".exe"
		}
		candidates = append(candidates, filepath.Join(base, "..", "libexec", "lark", native), filepath.Join(base, "libexec", "lark", native))
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

// Release pins are embedded by a verified signing pipeline, never loaded from
// user configuration, environment variables or an editable manifest.
var ReleaseCLIArtifactSHA256 string
var ReleaseCLIUpstreamSHA256 string
var officialCLIHashes = map[string]string{
	"darwin/amd64":  "cda6b35bd4d5fa2ae869627e748e1d27b3208847c40d19215ccdcccb7b48a1b4",
	"darwin/arm64":  "a8f652c2a62577a152467a983ec60548570fb0a08715e95fa3f579438c81885b",
	"linux/amd64":   "a2f55a5b7cb618f2635e1034f4e8196146c9c055c3995035322ba14382be66ba",
	"linux/arm64":   "0e30f6b4e1d0b887f4b6bf3391c747b5e14c11defc255930a59a7dfa23fabf92",
	"windows/amd64": "1b9babbff017bb5d4c9974f9ad547707aed61005196134b41e1e15764b213d74",
}

const officialCLIBinaryVCS = "8d8357d39dd15ac5e9aa815d4ab2639d56bd3239"

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, v := range s {
		if !((v >= '0' && v <= '9') || (v >= 'a' && v <= 'f')) {
			return false
		}
	}
	return true
}
func artifactAccepted(platform, sum, releaseArtifact, releaseUpstream string) bool {
	upstream, known := officialCLIHashes[platform]
	if !known {
		return false
	}
	if releaseArtifact != "" || releaseUpstream != "" {
		return validSHA(releaseArtifact) && releaseUpstream == upstream && sum == releaseArtifact
	}
	return sum == upstream
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
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if !artifactAccepted(platform, sum, ReleaseCLIArtifactSHA256, ReleaseCLIUpstreamSHA256) {
		return nil, sum, nil
	}
	out := []cloud.CapabilityGrant{}
	for _, kind := range []string{model.OperationBlockReplace, model.OperationBlockInsertAfter, model.OperationBlockDelete} {
		out = append(out, cloud.CapabilityGrant{Descriptor: model.OperationCapability{ID: "feishu-cli-1.0.99-" + kind, Kind: kind, Route: cloud.ShortcutXMLRoute}, CLISourceRevision: cloud.CLIRevision, Evidence: "protocol-live:darwin-arm64/2026-11-10-xml-subset-prepend0;protocol-live:darwin-arm64/2026-11-12-scheme3-nested-paragraph-and-code;official-release:1.0.99;binary-vcs:" + officialCLIBinaryVCS + ";inspected-source:" + cloud.InspectedSourceRevision + ";upstream-sha256:" + officialCLIHashes[platform] + ";artifact-sha256:" + sum + ";native-platform-not-verified", Verified: true})
	}
	return out, sum, nil
}

func isDrive(path string) bool {
	return runtime.GOOS == "windows" && len(path) >= 2 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (len(path) == 2 || path[2:] == `\` || path[2:] == "/")
}
