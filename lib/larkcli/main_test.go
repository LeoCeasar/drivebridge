package larkcli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// scenarioMarker sits next to a fake CLI executable on Windows. Windows cannot
// run the "#!/bin/sh" wrapper used elsewhere, and the managed runner strips the
// child's environment, so the scenario travels as a file beside the binary.
const scenarioMarker = "db-test-scenario"

// TestMain lets this test binary act as the fake official CLI. When it is started
// through a Windows test executable it runs only TestCLIProcess, with the CLI
// arguments after "--", exactly as the POSIX shell wrapper arranges.
func TestMain(m *testing.M) {
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), scenarioMarker)); err == nil {
			// Marker format: env var, test function and scenario, one per line.
			parts := strings.SplitN(string(b), "\n", 3)
			if len(parts) == 3 {
				_ = os.Setenv(parts[0], parts[2])
				os.Args = append([]string{os.Args[0], "-test.run=^" + parts[1] + "$", "--"}, os.Args[1:]...)
			}
		}
	}
	code := m.Run()
	if fakeCLIDir != "" {
		_ = os.RemoveAll(fakeCLIDir)
	}
	os.Exit(code)
}

// One private copy of the test binary per run. Links to the running test binary
// itself cannot be deleted on Windows, and a fresh copy per test would be scanned
// by antivirus each time; links to this copy avoid both.
var (
	fakeCLIOnce sync.Once
	fakeCLIDir  string
	fakeCLIPath string
	fakeCLIErr  error
)

func sharedFakeCLI(bin string) (string, error) {
	fakeCLIOnce.Do(func() {
		if fakeCLIDir, fakeCLIErr = os.MkdirTemp("", "larkcli-fake-"); fakeCLIErr != nil {
			return
		}
		fakeCLIPath = filepath.Join(fakeCLIDir, "lark-cli.exe")
		fakeCLIErr = copyFile(bin, fakeCLIPath)
	})
	return fakeCLIPath, fakeCLIErr
}

// windowsTestExecutable links a copy of the test binary into a private
// directory as lark-cli.exe and records the scenario beside it.
func windowsTestExecutable(t *testing.T, bin, scenario string) string {
	return windowsFakeCLI(t, bin, "DB_TEST_SCENARIO", "TestCLIProcess", scenario)
}

func windowsFakeCLI(t *testing.T, bin, envName, testName, scenario string) string {
	t.Helper()
	shared, err := sharedFakeCLI(bin)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "lark-cli.exe")
	if err := os.Link(shared, exe); err != nil {
		if err := copyFile(shared, exe); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, scenarioMarker), []byte(envName+"\n"+testName+"\n"+scenario), 0600); err != nil {
		t.Fatal(err)
	}
	return exe
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
