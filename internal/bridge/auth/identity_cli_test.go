package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

// TestMain makes a private copy of this binary a portable, deterministic CLI.
// The production protocol runs unchanged; no shell or inherited fixture env is used.
func TestMain(m *testing.M) {
	if exe, err := os.Executable(); err == nil {
		if body, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "drivebridge-identity-fixture.json")); err == nil {
			if len(os.Args) > 1 && os.Args[1] == "config" {
				fmt.Print(`{"appId":"app","brand":"feishu"}`)
			} else {
				fmt.Print(string(body))
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}
func identityCLI(t *testing.T, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	name := "lark-cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	target := filepath.Join(dir, name)
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal(copyErr, closeErr)
	}
	if err = os.WriteFile(filepath.Join(dir, "drivebridge-identity-fixture.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	return target
}
func TestBoundIdentityMismatchPrecedesCLICredentialFailure(t *testing.T) {
	for _, method := range []string{"status", "reconcile"} {
		for _, identity := range []string{"user", "brand"} {
			for _, state := range []string{"missing", "storage", "verification"} {
				t.Run(method+"/"+identity+"/"+state, func(t *testing.T) {
					s, _, f, id, _ := fixture(t)
					enroll(t, s, f, id)
					login(t, s, f, id)
					if method == "reconcile" {
						allowGuard(s)
						freshUserAuth(f, "user")
						interrupt(t, s, id)
					}
					brand, user := "feishu", "user"
					if identity == "user" {
						user = "other-user"
					} else {
						brand = "lark"
					}
					u := map[string]any{"openId": user, "available": false, "status": "missing"}
					if state == "storage" {
						u["status"] = "error"
						u["error"] = map[string]any{"type": "internal", "subtype": "storage"}
					}
					if state == "verification" {
						u["status"] = "verify_failed"
						u["verified"] = false
						u["error"] = map[string]any{"type": "network", "subtype": "transport"}
					}
					body, _ := json.Marshal(map[string]any{"appId": "app", "brand": brand, "identities": map[string]any{"user": u}})
					executable := identityCLI(t, body)
					s.factory = func(o larkcli.EnrollmentOptions) (protocol, error) {
						o.Executable = executable
						r, err := larkcli.NewEnrollment(o)
						return cliProtocol{r: r, options: o}, err
					}
					var err error
					if method == "status" {
						_, err = s.Status(context.Background(), id)
					} else {
						_, err = s.Reconcile(context.Background(), id)
					}
					if !errors.Is(err, model.ErrIdentityChanged) {
						t.Fatalf("bound %s mismatch became a credential failure: %v", identity, err)
					}
				})
			}
		}
	}
}
