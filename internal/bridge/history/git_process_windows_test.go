package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const gitProcessHelperEnv = "DRIVEBRIDGE_HISTORY_PROCESS_HELPER"
const gitProcessBody = "history body\n"

// The test executable stands in for Git so the runtime probe's actual launch
// can be checked without adding a command-injection seam to production code.
func TestMain(m *testing.M) {
	if os.Getenv(gitProcessHelperEnv) != "1" {
		os.Exit(m.Run())
	}
	var startup windows.StartupInfo
	if err := windows.GetStartupInfo(&startup); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if startup.Flags&windows.STARTF_USESHOWWINDOW == 0 || startup.ShowWindow != windows.SW_HIDE {
		fmt.Fprintln(os.Stderr, "history Git child was not launched with HideWindow")
		os.Exit(2)
	}
	console, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	if console != 0 {
		fmt.Fprintln(os.Stderr, "history Git child has a console")
		os.Exit(2)
	}
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Fprintln(os.Stdout, "git version 2.50.1")
	case len(args) > 0 && args[len(args)-1] == "echo-stdin":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	case len(args) >= 3 && args[len(args)-3] == "cat-file" && args[len(args)-2] == "blob":
		fmt.Fprint(os.Stdout, gitProcessBody)
	default:
		fmt.Fprintf(os.Stderr, "unexpected helper arguments: %q\n", args)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestHistoryGitCommandSuppressesConsoleWithoutDetaching(t *testing.T) {
	r := &Repository{executable: `C:\Program Files\Git\bin\git.exe`, gitDir: t.TempDir()}
	cmd := r.gitCommand(context.Background(), nil, "status")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 || cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Fatal("history Git child must suppress its console without detaching pipes or cancellation")
	}
}

func TestHistoryGitProcessesKeepPipesWithoutConsole(t *testing.T) {
	t.Setenv(gitProcessHelperEnv, "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := &Repository{executable: executable, gitDir: t.TempDir()}
	t.Run("version probe", func(t *testing.T) {
		got, err := resolveGit(ctx, executable)
		if err != nil || got != executable {
			t.Fatalf("hidden version probe: %q %v", got, err)
		}
	})
	t.Run("bounded archive command", func(t *testing.T) {
		out, err := r.git(ctx, strings.NewReader(gitProcessBody), "echo-stdin")
		if err != nil || string(out) != gitProcessBody {
			t.Fatalf("hidden command stdin/stdout: %q %v", out, err)
		}
	})
	t.Run("streamed body", func(t *testing.T) {
		digest := sha256.Sum256([]byte(gitProcessBody))
		stream, err := r.openGitBody(ctx, Ref{OID: strings.Repeat("a", 40), SHA256: hex.EncodeToString(digest[:]), Size: int64(len(gitProcessBody))})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		out, err := io.ReadAll(stream)
		if err != nil || string(out) != gitProcessBody {
			t.Fatalf("hidden body stdout: %q %v", out, err)
		}
	})
}
