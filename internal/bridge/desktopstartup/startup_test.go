package desktopstartup

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, goos string) (backend, Spec) {
	t.Helper()
	d := t.TempDir()
	return backend{goos: goos, home: d, config: filepath.Join(d, "xdg"), uid: "test-user", run: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}, Spec{filepath.Join(d, "App & special", "drivebridge-desktop"), filepath.Join(d, "config $ x%.json"), filepath.Join(d, "lark-cli")}
}
func TestLaunchAgentLiteralArguments(t *testing.T) {
	b, s := fixture(t, "darwin")
	data, err := b.render(s)
	if err != nil {
		t.Fatal(err)
	}
	var plist struct {
		Dict struct {
			Args []string `xml:"array>string"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(data, &plist); err != nil {
		t.Fatal(err)
	}
	want := []string{s.Executable, "desktop", "_run", "--bridge-config", s.ConfigFile, "--lark-cli", s.CLIExecutable}
	if !reflect.DeepEqual(plist.Dict.Args, want) {
		t.Fatalf("%q", plist.Dict.Args)
	}
	if !strings.Contains(string(data), "<key>KeepAlive</key><false/>") {
		t.Fatal("unexpected supervisor")
	}
}
func TestLinuxDesktopSessionRegistrationAndIdentity(t *testing.T) {
	b, s := fixture(t, "linux")
	ctx := context.Background()
	if err := b.set(ctx, s, true); err != nil {
		t.Fatal(err)
	}
	on, err := b.enabled(ctx, s)
	if !on || err != nil {
		t.Fatalf("%v %v", on, err)
	}
	data, _ := os.ReadFile(b.path())
	if !strings.Contains(string(data), "%%") || strings.Contains(string(data), "systemd") {
		t.Fatal(string(data))
	}
	s.ConfigFile += "other"
	if _, err := b.enabled(ctx, s); err == nil {
		t.Fatal("cross configuration accepted")
	}
	if err := b.set(ctx, s, false); err == nil {
		t.Fatal("cross configuration removed")
	}
	s.ConfigFile = strings.TrimSuffix(s.ConfigFile, "other")
	if err := b.set(ctx, s, false); err != nil {
		t.Fatal(err)
	}
	on, err = b.enabled(ctx, s)
	if on || err != nil {
		t.Fatalf("%v %v", on, err)
	}
}
func TestMacRegistrationLeavesRunningDesktopAlone(t *testing.T) {
	b, s := fixture(t, "darwin")
	b.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("must not load or unload live desktop")
		return nil, nil
	}
	for _, enabled := range []bool{true, true, false, false} {
		if err := b.set(context.Background(), s, enabled); err != nil {
			t.Fatal(err)
		}
		got, err := b.enabled(context.Background(), s)
		if err != nil || got != enabled {
			t.Fatalf("enabled=%v err=%v", got, err)
		}
	}
}
func TestWindowsTaskRestrictedToCurrentInteractiveUser(t *testing.T) {
	b, s := fixture(t, "windows")
	data, err := b.render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"<UserId>test-user</UserId>", "<LogonType>InteractiveToken</LogonType>", "<RunLevel>LeastPrivilege</RunLevel>", "<Hidden>true</Hidden>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>"} {
		if !strings.Contains(string(data), text) {
			t.Fatal(text)
		}
	}
	on, err := taskMatches(data, data)
	if !on || err != nil {
		t.Fatalf("%v %v", on, err)
	}
	wrong := strings.ReplaceAll(string(data), "test-user", "SYSTEM")
	if _, err := taskMatches([]byte(wrong), data); err == nil {
		t.Fatal("wrong principal accepted")
	}
	if got := windowsQuote(`C:\trailing\`); got != `"C:\trailing\\"` {
		t.Fatal(got)
	}
}
func TestWindowsRegistrationUsesXMLFileAndNoShell(t *testing.T) {
	b, s := fixture(t, "windows")
	b.run = func(_ context.Context, cmd string, args ...string) ([]byte, error) {
		if cmd != "schtasks.exe" {
			t.Fatal(cmd)
		}
		if args[0] == "/Query" {
			return nil, nil
		}
		if args[0] != "/Create" {
			t.Fatal(args)
		}
		data, err := os.ReadFile(args[4])
		if err != nil {
			t.Fatal(err)
		}
		expected, _ := b.render(s)
		if !reflect.DeepEqual(data, expected) {
			t.Fatal("different task")
		}
		return nil, nil
	}
	if err := b.set(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
}
func TestInvalidSpecNeverRuns(t *testing.T) {
	b, s := fixture(t, "linux")
	s.ConfigFile = "relative"
	if err := b.set(context.Background(), s, true); err == nil {
		t.Fatal("relative config accepted")
	}
}

func TestArgumentQuotingVectors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{``, `""`}, {`a b`, `"a b"`}, {`a"b`, `"a\"b"`}, {`C:\path with space\`, `"C:\path with space\\"`},
	} {
		if got := windowsQuote(tc.in); got != tc.want {
			t.Errorf("Windows %q got %q want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{`a b`, `"a b"`}, {`a%b`, `"a%%b"`}, {`a\b`, `"a\\\\b"`}, {`a"b`, `"a\\"b"`}, {`a$b`, `"a\\$b"`},
	} {
		if got := desktopQuote(tc.in); got != tc.want {
			t.Errorf("Desktop %q got %q want %q", tc.in, got, tc.want)
		}
	}
}
func TestWindowsQueryUsesUniqueUserTaskAndPreservesErrors(t *testing.T) {
	b, s := fixture(t, "windows")
	expected, _ := b.render(s)
	b.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] == "CSV" {
			return []byte(`"\DriveBridge Desktop-other-user","N/A","Ready"` + "\n" + `"\DriveBridge Desktop-test-user","N/A","Ready"`), nil
		}
		if args[2] != b.taskName() {
			t.Fatal(args)
		}
		return expected, nil
	}
	on, err := b.enabled(context.Background(), s)
	if !on || err != nil {
		t.Fatalf("%v %v", on, err)
	}
	b.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("permission denied") }
	if _, err := b.enabled(context.Background(), s); err == nil {
		t.Fatal("query permission failure hidden as disabled")
	}
}
