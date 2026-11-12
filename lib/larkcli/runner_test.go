package larkcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCLIProcess(t *testing.T) {
	scenario := os.Getenv("DB_TEST_SCENARIO")
	if scenario == "" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	emit := func(data string) { fmt.Print(`{"ok":true,"identity":"user","data":` + data + `}`) }
	switch scenario {
	case "logout", "logout-not-logged", "logout-malformed", "logout-partial":
		if strings.Join(args, " ") != "auth logout --json" {
			os.Exit(99)
		}
		switch scenario {
		case "logout":
			fmt.Print(`{"ok":true,"loggedOut":true}`)
		case "logout-not-logged":
			fmt.Print(`{"ok":true,"loggedOut":false,"reason":"not_logged_in"}`)
		case "logout-malformed":
			fmt.Print(`{"ok":true}`)
		case "logout-partial":
			fmt.Print(`{"ok":true,"loggedOut":true}`)
			fmt.Fprint(os.Stderr, "SECRET")
			os.Exit(1)
		}

	case "config-metadata":
		fmt.Print(`{"appId":"cli_app","brand":"feishu","appSecret":"SECRET"}`)
	case "identity-metadata":
		fmt.Print(`{"appId":"cli_app","brand":"feishu","identities":{"user":{"openId":"user","verified":true,"available":true,"scope":"doc:read","expiresAt":"2030-01-01T00:00:00Z"}}}`)
	case "enrollment", "enrollment-failed":
		fmt.Fprint(os.Stderr, "Scan code\n  https://open.feishu.cn/page/cli?user_code=SAFE\n")
		time.Sleep(150 * time.Millisecond)
		fmt.Print("{\n  \"appId\": \"cli_app\",\n  \"brand\": \"feishu\",\n  \"appSecret\": \"SECRET\"\n}")
		if scenario == "enrollment-failed" {
			os.Exit(1)
		}
	case "auth-incomplete", "auth-missing":
		fmt.Println(`{"event":"device_authorization","verification_uri_complete":"https://accounts.feishu.cn/verify?user_code=SAFE","user_code":"SAFE","expires_in":600}`)
		time.Sleep(100 * time.Millisecond)
		if scenario == "auth-incomplete" {
			fmt.Println(`{"event":"authorization_complete","user_open_id":"user"}`)
		} else {
			fmt.Println(`{"event":"authorization_complete","user_open_id":"user","scope":"","requested":["doc:read"],"granted":[],"missing":["doc:read"]}`)
			os.Exit(1)
		}
	case "auth":
		fmt.Println(`{"event":"device_authorization","verification_uri":"https://accounts.feishu.cn/verify","verification_uri_complete":"https://accounts.feishu.cn/verify?user_code=SAFE","user_code":"SAFE","expires_in":600,"device_code":"SECRET"}`)
		time.Sleep(150 * time.Millisecond)
		fmt.Println(`{"event":"authorization_complete","user_open_id":"user","scope":"doc:read","requested":["doc:read"],"granted":["doc:read"],"missing":[],"expires_at":"2030-01-01T00:00:00Z"}`)
	case "stderr":
		fmt.Fprint(os.Stderr, `{"ok":false,"identity":"user","error":{"type":"api","subtype":"rate_limit","code":429,"message":"SECRET BODY","retryable":true,"retry_after_seconds":3}}`)
		os.Exit(1)
	case "partial":
		fmt.Print(`{"ok":false,"identity":"user","data":{"result":"failed","warnings":["partial"]}}`)
		os.Exit(1)
	case "echo":
		b, _ := io.ReadAll(os.Stdin)
		v, _ := json.Marshal(map[string]any{"args": args, "env": os.Environ(), "stdin": string(b)})
		emit(string(v))
	case "cancel":
		time.Sleep(time.Minute)
	case "invalid":
		fmt.Print(`{"ok":true,"identity":"tenant","data":{}}`)
	default:
		if handleDocxProcess(scenario, args, emit) {
			break
		}
		if handleDriveProcess(scenario, args, emit) {
			break
		}
		emit(`{}`)
	}
	os.Exit(0)
}
func testExecutable(t *testing.T, scenario string) string {
	t.Helper()
	bin, _ := os.Executable()
	if runtime.GOOS == "windows" {
		return windowsTestExecutable(t, bin, scenario)
	}
	p := filepath.Join(t.TempDir(), "cli")
	script := "#!/bin/sh\nDB_TEST_SCENARIO='" + scenario + "' exec '" + strings.ReplaceAll(bin, "'", "'\\''") + "' -test.run=TestCLIProcess -- \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestRunnerErrorEnvelopeOnStderr(t *testing.T) {
	r := NewDirect(testExecutable(t, "stderr"))
	_, err := r.Run(context.Background(), Request{Args: []string{"drive", "files", "list"}})
	var e *Error
	if !errors.As(err, &e) || e.Code != "429" || !e.Retryable || e.RetryAfter != 3*time.Second {
		t.Fatalf("error=%#v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("raw error leaked")
	}
}
func TestPartialSuccessNotConfirmed(t *testing.T) {
	r := NewDirect(testExecutable(t, "partial"))
	v, err := r.Run(context.Background(), Request{})
	if err == nil || v.Result != "failed" || len(v.Warnings) != 1 || len(v.Data) == 0 {
		t.Fatalf("%+v %v", v, err)
	}
}
func TestAuthOutputRedacted(t *testing.T) {
	r := NewDirect(testExecutable(t, "stderr"))
	_, err := r.Run(context.Background(), Request{Args: []string{"secret-body"}, Stdin: []byte("SECRET")})
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

type testCoordinator struct {
	mu     sync.Mutex
	active bool
	calls  int
}

func (c *testCoordinator) WithCredentialLocks(ctx context.Context, a, u string, run func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = true
	c.calls++
	defer func() { c.active = false }()
	return run()
}
func (c *testCoordinator) WithEnrollmentLock(ctx context.Context, run func() error) error {
	return c.WithCredentialLocks(ctx, "", "", run)
}
func TestManagedRejectsDowngradeAndSanitizesEnvironment(t *testing.T) {
	if _, err := NewManaged(ManagedOptions{Executable: "x"}); err == nil {
		t.Fatal("missing identity accepted")
	}
	t.Setenv("LARKSUITE_CLI_CONFIG_DIR", "wrong")
	t.Setenv("FEISHU_APP_SECRET", "secret")
	t.Setenv("OPENCLAW_HOME", "bad")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	t.Setenv("SSL_CERT_FILE", "/cert")
	c := new(testCoordinator)
	root := t.TempDir()
	r, err := NewManaged(ManagedOptions{Executable: testExecutable(t, "echo"), AuthRef: "opaque", AuthRoot: root, Identity: Identity{AppID: "app", UserOpenID: "user", Brand: "feishu"}, Coordinator: c})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(context.Background(), Request{Args: []string{"drive", "files", "list"}, Stdin: []byte("hello")})
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Args, Env []string
		Stdin     string
	}
	json.Unmarshal(out.Data, &data)
	joined := strings.Join(data.Env, "\n")
	if c.calls != 1 || !strings.Contains(joined, "LARKSUITE_CLI_CONFIG_DIR="+root) || strings.Contains(joined, "FEISHU_APP_SECRET") || strings.Contains(joined, "OPENCLAW_HOME") || !strings.Contains(joined, "HTTPS_PROXY=http://proxy.invalid") || !strings.Contains(joined, "SSL_CERT_FILE=/cert") || !strings.Contains(strings.Join(data.Args, " "), "--as user") || data.Stdin != "hello" {
		t.Fatalf("calls=%d %+v", c.calls, data)
	}
}
func TestRunnerCancellationAndIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := NewDirect(testExecutable(t, "cancel")).Run(ctx, Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	_, err = NewDirect(testExecutable(t, "invalid")).Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("wrong identity accepted")
	}
}

func TestRetainedEnrollmentAndAuthLifecycle(t *testing.T) {
	for _, kind := range []string{"enrollment", "auth"} {
		t.Run(kind, func(t *testing.T) {
			c := new(testCoordinator)
			r, err := NewEnrollment(EnrollmentOptions{Executable: testExecutable(t, kind), AuthRef: "ref", AuthRoot: t.TempDir(), Coordinator: c, AppID: "app"})
			if err != nil {
				t.Fatal(err)
			}
			var stream *AuthStream
			if kind == "enrollment" {
				stream, err = r.StartEnrollment(context.Background(), "feishu")
			} else {
				stream, err = r.StartAuth(context.Background(), []string{"doc:read"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if stream.Initial.URL == "" {
				t.Fatal("no URL")
			}
			result, err := stream.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if kind == "auth" && result.UserOpenID != "user" {
				t.Fatalf("%+v", result)
			}
			if kind == "enrollment" && result.AppID != "cli_app" {
				t.Fatalf("%+v", result)
			}
			if _, err = stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(fmt.Sprint(result), "SECRET") {
				t.Fatal("secret retained")
			}
			if _, err = r.Run(context.Background(), Request{}); err == nil {
				t.Fatal("enrollment runner allowed business request")
			}
		})
	}
}
func TestRetainedCancelReapsBeforeUnlock(t *testing.T) {
	c := new(testCoordinator)
	r, _ := NewEnrollment(EnrollmentOptions{Executable: testExecutable(t, "auth"), AuthRef: "ref", AuthRoot: t.TempDir(), Coordinator: c, AppID: "app"})
	stream, err := r.StartAuth(context.Background(), []string{"doc:read"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.CancelAndWait()
	if err == nil {
		t.Fatal("cancel confirmed success")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active {
		t.Fatal("lock not released after reap")
	}
}

func TestEnvelopeRejectsContradictoryOrMalformedSuccess(t *testing.T) {
	for _, s := range []string{`{}`, `{"ok":null,"identity":"user","data":{}}`, `{"ok":"true","identity":"user","data":{}}`, `{"ok":true,"identity":"user","data":{}} {}`, `{"ok":true,"identity":"user","data":null}`, `{"ok":true,"identity":"user","data":{"result":"pending"}}`} {
		if _, err := decodeResponse([]byte(s), nil, nil); err == nil {
			t.Fatal("accepted", s)
		}
	}
	v, err := decodeResponse([]byte(`{"ok":true,"identity":"user","data":{"warnings":["preserve"]}}`), []byte(`{"ok":false,"identity":"user","error":{"type":"api","code":1}}`), nil)
	if err == nil || len(v.Warnings) != 1 {
		t.Fatal(v, err)
	}
}
func TestManagedCannotOverrideBoundConfiguration(t *testing.T) {
	r, _ := NewManaged(ManagedOptions{Executable: testExecutable(t, "echo"), AuthRef: "ref", AuthRoot: t.TempDir(), Identity: Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: new(testCoordinator)})
	for _, args := range [][]string{{"drive", "--profile=other"}, {"drive", "--workspace=other"}, {"drive", "--as=user"}, {"drive", "--config-dir=other"}} {
		if _, err := r.Run(context.Background(), Request{Args: args}); err == nil {
			t.Fatal("override accepted", args)
		}
	}
}
func TestLifecycleMetadataUsesBareWhitelistedDTOs(t *testing.T) {
	r, _ := NewEnrollment(EnrollmentOptions{Executable: testExecutable(t, "config-metadata"), AuthRef: "ref", AuthRoot: t.TempDir(), AppID: "cli_app", Coordinator: new(testCoordinator)})
	config, err := r.InspectConfig(context.Background())
	if err != nil || config.AppID != "cli_app" || strings.Contains(fmt.Sprint(config), "SECRET") {
		t.Fatal(config, err)
	}
	r, _ = NewEnrollment(EnrollmentOptions{Executable: testExecutable(t, "identity-metadata"), AuthRef: "ref", AuthRoot: t.TempDir(), AppID: "cli_app", Coordinator: new(testCoordinator)})
	id, err := r.VerifyIdentity(context.Background())
	if err != nil || id.UserOpenID != "user" || !id.Verified {
		t.Fatal(id, err)
	}
}

func TestStderrWarningAndPrettyErrorEnvelope(t *testing.T) {
	stderr := []byte("warning: sensitive prose\n{\n  \"ok\": false,\n  \"identity\": \"user\",\n  \"error\": {\"type\": \"api\", \"code\": 429, \"retryable\": true}\n}\n")
	_, err := decodeResponse(nil, stderr, errors.New("exit"))
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "429" || !typed.Retryable {
		t.Fatal(err)
	}
}

func TestAuthCompletionRequiresScopePresenceAndPreservesFailure(t *testing.T) {
	for _, s := range []string{"auth-incomplete", "auth-missing"} {
		r, _ := NewEnrollment(EnrollmentOptions{Executable: testExecutable(t, s), AuthRef: "ref", AuthRoot: t.TempDir(), AppID: "app", Coordinator: new(testCoordinator)})
		stream, err := r.StartAuth(context.Background(), []string{"doc:read"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := stream.Wait()
		if err == nil {
			t.Fatal("incomplete authorization accepted")
		}
		if s == "auth-missing" && (result.UserOpenID != "user" || len(result.Missing) != 1) {
			t.Fatal("partial completion lost", result, err)
		}
	}
}

func TestResultPresenceCannotBecomeMissingSuccess(t *testing.T) {
	for _, data := range []string{`{"result":null}`, `{"result":""}`, `{"result":1}`, `{"warnings":"bad"}`} {
		if _, err := decodeResponse([]byte(`{"ok":true,"identity":"user","data":`+data+`}`), nil, nil); err == nil {
			t.Fatal("accepted", data)
		}
	}
}

func TestManagedRejectsTypedNilCoordinator(t *testing.T) {
	var c *testCoordinator
	if _, err := NewManaged(ManagedOptions{Executable: "unused", AuthRef: "ref", AuthRoot: t.TempDir(), Identity: Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: c}); err == nil {
		t.Fatal("nil lock implementation accepted")
	}
}
func TestRegistrationURLValidation(t *testing.T) {
	for _, u := range []string{"http://open.feishu.cn/page/cli?user_code=x", "https://open.feishu.cn.evil.invalid/page/cli?user_code=x", "https://open.feishu.cn/page/cli", "https://open.feishu.cn:443/page/cli?user_code=x", "https://user@open.feishu.cn/page/cli?user_code=x"} {
		if officialURL(u, true) {
			t.Fatal("unsafe registration URL accepted", u)
		}
	}
}

func TestManagedWindowsEnvironmentKeys(t *testing.T) {
	env := managedEnvironment([]string{"Path=C:\\tools", "SystemRoot=C:\\Windows", "Https_Proxy=http://proxy.invalid", "Larksuite_Cli_Profile=other", "Feishu_App_Secret=secret"}, "C:\\auth", true)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "Path=C:\\tools") || !strings.Contains(joined, "SystemRoot=C:\\Windows") || !strings.Contains(joined, "Https_Proxy=http://proxy.invalid") || strings.Contains(joined, "secret") || strings.Contains(joined, "other") {
		t.Fatal("Windows environment policy did not preserve only allowed keys")
	}
}

func TestEnrollmentOutputBeforeProbeFailureIsNotSuccess(t *testing.T) {
	r, _ := NewEnrollment(EnrollmentOptions{Executable: testExecutable(t, "enrollment-failed"), AuthRef: "ref", AuthRoot: t.TempDir(), Coordinator: new(testCoordinator)})
	stream, err := r.StartEnrollment(context.Background(), "feishu")
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err == nil || result.AppID != "cli_app" {
		t.Fatal("saved configuration failure lost", result, err)
	}
}

func TestUnresolvedStderrIdentityStillPreservesTypedFailure(t *testing.T) {
	_, err := decodeResponse(nil, []byte(`{"ok":false,"error":{"type":"authorization","subtype":"not_configured","code":999,"retryable":true}}`), errors.New("exit"))
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "999" || !typed.Retryable {
		t.Fatal("unresolved typed failure discarded", err)
	}
}
