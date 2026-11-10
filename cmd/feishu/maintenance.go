package feishu

import (
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
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
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
	if len(args) == 0 {
		return manager.ErrConfig
	}
	return lease(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		proof, closeProof, err := startInstallationProof()
		if err != nil {
			return err
		}
		defer closeProof()
		child := exec.Command(args[0], args[1:]...)
		child.Stdin = stdin
		child.Stdout = stdout
		child.Stderr = stderr
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(entry), installationProofEnv+"=") {
				child.Env = append(child.Env, entry)
			}
		}
		child.Env = append(child.Env, installationProofEnv+"="+proof)
		if err := child.Start(); err != nil {
			return err
		}
		return child.Wait()
	})
}

// The proof is a short-lived, same-user, authenticated loopback endpoint. It is
// only created inside the held lease; no editable on-disk success marker exists.
// Elevated/cross-user installer contexts fail closed. Native environment and
// impersonation behavior still require platform-specific installation evidence.
func startInstallationProof() (string, func(), error) {
	current, err := user.Current()
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
	return base64.RawURLEncoding.EncodeToString(raw), func() { _ = server.Close() }, nil
}
func verifyInstallationProof(ctx context.Context, encoded string) error {
	fail := errors.New("installation requires a live same-user maintenance exec lease")
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
	current, err := user.Current()
	if err != nil || p.User != current.Uid {
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
