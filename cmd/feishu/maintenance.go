package feishu

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/spf13/cobra"
)

const installationProofEnv = "DRIVEBRIDGE_INSTALL_LEASE"

type installationProof struct {
	Address string `json:"address"`
	Token   string `json:"token"`
	User    string `json:"user"`
}

func installMaintenanceCommands(root *cobra.Command, o CommandOptions) {
	maintenance := &cobra.Command{Use: "maintenance", Hidden: true}
	run := &cobra.Command{Use: "exec -- installer [args...]", Args: func(c *cobra.Command, args []string) error {
		if c.ArgsLenAtDash() != 0 || len(args) == 0 {
			return fmt.Errorf("installer argv must follow --")
		}
		return nil
	}}
	run.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		return nil, executeInstallation(ctx, m.WithInstallationLease, args, run.InOrStdin(), run.OutOrStdout(), run.ErrOrStderr())
	})
	verify := &cobra.Command{Use: "verify", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return verifyInstallationProof(c.Context(), os.Getenv(installationProofEnv))
	}}
	maintenance.AddCommand(run, verify)
	root.AddCommand(maintenance)
}

// A synchronous child owns the callback lifetime. CommandContext is deliberately
// not used: cancellation after Start must not release ownership while replacement
// is still running. Installers that detach a second process are unsupported.
func executeInstallation(ctx context.Context, lease func(context.Context, func() error) error, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return executeInstallationAs(ctx, lease, args, stdin, stdout, stderr, canonicalInstallationUser)
}

func executeInstallationAs(ctx context.Context, lease func(context.Context, func() error) error, args []string, stdin io.Reader, stdout, stderr io.Writer, identity installationIdentity) error {
	if len(args) == 0 {
		return manager.ErrConfig
	}
	return lease(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, closeProof, err := startInstallationProofAs(identity)
		if err != nil {
			return err
		}
		defer closeProof()
		child := exec.Command(args[0], args[1:]...)
		child.Stdin = stdin
		child.Stdout = stdout
		child.Stderr = stderr
		child.Env = []string{}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(entry), installationProofEnv+"=") {
				child.Env = append(child.Env, entry)
			}
		}
		if err := child.Start(); err != nil {
			return err
		}
		return child.Wait()
	})
}

// The descriptor only locates a short-lived authenticated loopback endpoint; it
// never establishes success by itself. The protocol is cooperative within one OS
// account, not an anti-tamper boundary against that same account.
type installationIdentity func() (*user.User, error)

// os/user uses the native account database on macOS and the process token/profile
// on Windows. On other Unix builds Current can fall back to HOME if lookup fails;
// require a passwd entry there instead of accepting that environment fallback.
func canonicalInstallationUser() (*user.User, error) {
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if os.Getuid() != os.Geteuid() || current.Uid != strconv.Itoa(os.Getuid()) {
			return nil, manager.ErrConfig
		}
		if runtime.GOOS != "darwin" {
			raw, err := os.ReadFile("/etc/passwd")
			if err != nil {
				return nil, err
			}
			found := false
			for _, line := range strings.Split(string(raw), "\n") {
				fields := strings.Split(line, ":")
				if len(fields) == 7 && fields[2] == current.Uid {
					current.HomeDir = fields[5]
					found = true
					break
				}
			}
			if !found {
				return nil, manager.ErrConfig
			}
		}
	}
	if current.Uid == "" || !filepath.IsAbs(current.HomeDir) || filepath.Clean(current.HomeDir) != current.HomeDir {
		return nil, manager.ErrConfig
	}
	return current, nil
}

func installationDescriptorPath(current *user.User) string {
	return filepath.Join(current.HomeDir, ".drivebridge-install", "lease.json")
}

func startInstallationProofAs(identity installationIdentity) (string, func(), error) {
	current, err := identity()
	if err != nil {
		return "", nil, err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	p := installationProof{Address: "http://" + listener.Addr().String(), Token: hex.EncodeToString(secret), User: current.Uid}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/lease" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.Token)) != 1 {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	})}
	go func() { _ = server.Serve(listener) }()
	raw, _ := json.Marshal(p)
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	cleanup, err := publishInstallationDescriptor(installationDescriptorPath(current), []byte(encoded))
	if err != nil {
		_ = server.Close()
		return "", nil, err
	}
	return encoded, func() { _ = server.Close(); cleanup() }, nil
}
func verifyInstallationProof(ctx context.Context, encoded string) error {
	return verifyInstallationProofAs(ctx, encoded, canonicalInstallationUser)
}

func verifyInstallationProofAs(ctx context.Context, encoded string, identity installationIdentity) error {
	fail := errors.New("installation requires a live same-user maintenance exec lease")
	current, err := identity()
	if err != nil {
		return fail
	}
	if encoded == "" {
		raw, err := readInstallationDescriptor(installationDescriptorPath(current))
		if err != nil {
			return fail
		}
		encoded = string(raw)
	}
	if len(encoded) > 2048 {
		return fail
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return fail
	}
	var p installationProof
	if json.Unmarshal(raw, &p) != nil {
		return fail
	}
	secret, err := hex.DecodeString(p.Token)
	if err != nil || len(secret) != 32 {
		return fail
	}
	if p.User != current.Uid {
		return fail
	}
	address, err := url.Parse(p.Address)
	if err != nil || address.Scheme != "http" || address.Hostname() != "127.0.0.1" || address.Port() == "" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
		return fail
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", p.Address+"/lease", nil)
	if err != nil {
		return fail
	}
	request.Header.Set("Authorization", "Bearer "+p.Token)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return fail }}
	response, err := client.Do(request)
	if err != nil {
		return fail
	}
	defer response.Body.Close()
	var actual installationProof
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 2048)).Decode(&actual) != nil || actual != p {
		return fail
	}
	return nil
}

// Publication is atomic and durable. Cleanup checks the published inode and bytes
// so an older holder cannot remove a replacement descriptor or follow a symlink.
func publishInstallationDescriptor(path string, raw []byte) (func(), error) {
	dir := filepath.Dir(path)
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	if err := platform.CheckNoSymlinks(path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, manager.ErrConfig
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".lease-*")
	if err != nil {
		return nil, err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	defer f.Close()
	if err = platform.SecurePrivate(temporary, false); err != nil {
		return nil, err
	}
	if _, err = f.Write(raw); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	published, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	cleanup := func() {
		if platform.CheckNoSymlinks(path) != nil {
			return
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(published, current) {
			return
		}
		actual, err := readInstallationDescriptor(path)
		if err != nil || !bytes.Equal(actual, raw) {
			return
		}
		if os.Remove(path) == nil {
			_ = platform.SyncDir(dir)
		}
	}
	if err = platform.CheckNoSymlinks(path); err != nil {
		return nil, err
	}
	if err = platform.ReplaceFile(temporary, path); err != nil {
		return nil, err
	}
	if err = platform.SyncDir(dir); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func readInstallationDescriptor(path string) ([]byte, error) {
	if err := platform.CheckNoSymlinks(path); err != nil {
		return nil, err
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || runtime.GOOS != "windows" && dir.Mode().Perm() != 0700 {
		return nil, manager.ErrConfig
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2048 || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		return nil, manager.ErrConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, manager.ErrConfig
	}
	return io.ReadAll(io.LimitReader(f, 2049))
}
