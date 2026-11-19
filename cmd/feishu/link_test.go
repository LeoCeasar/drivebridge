package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/spf13/cobra"
)

func TestLinkDeadlineCoversManagerConstruction(t *testing.T) {
	started := time.Now()
	c := newLinkCommand(CommandOptions{NewManager: func(c *cobra.Command) (*manager.Manager, error) {
		<-c.Context().Done()
		return nil, c.Context().Err()
	}}, linkActions{})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SetArgs([]string{"--path", "/selected/report.md", "--timeout", "20ms"})
	err := c.Execute()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatal(err)
	}
}

func TestLinkCommandSingleSelectionAndClipboardOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		lookupErr error
		wantCopy  bool
	}{
		{"single", []string{"--path", "/selected/方案.md", "--copy", "--json"}, nil, true},
		{"lookup fails", []string{"--path", "/selected/方案.md", "--copy", "--json"}, manager.ErrLinkNotUploaded, false},
		{"extra path", []string{"--path", "/selected/a", "/selected/b", "--copy"}, nil, false},
		{"repeated path flag", []string{"--path", "/selected/a", "--path", "/selected/b", "--copy"}, nil, false},
		{"missing path", []string{"--copy"}, nil, false},
		{"json only", []string{"--path", "/selected/方案.md", "--json"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copied, feedback := false, ""
			a := linkActions{
				resolve: func(_ context.Context, _ *manager.Manager, p string) (manager.LinkResult, error) {
					return manager.LinkResult{SchemaVersion: 1, URL: "https://tenant.feishu.cn/docx/doc", PendingChanges: true}, tc.lookupErr
				},
				copy: func(s string) error {
					copied = true
					if s != "https://tenant.feishu.cn/docx/doc" {
						t.Fatal(s)
					}
					return nil
				},
				feedback: func(_ context.Context, message string, failed bool) { feedback = message },
			}
			root := newLinkCommand(CommandOptions{NewManager: func(*cobra.Command) (*manager.Manager, error) {
				return manager.New(manager.Options{ConfigFile: configFixture(t)})
			}}, a)
			root.Flags().Bool("json", false, "")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tc.args)
			err := root.Execute()
			valid := tc.name == "single" || tc.name == "json only"
			if (err == nil) != valid || copied != tc.wantCopy {
				t.Fatalf("copied=%v err=%v", copied, err)
			}
			if tc.wantCopy && !strings.Contains(feedback, "待同步") {
				t.Fatal("missing pending change notice", feedback)
			}
			if valid {
				var result manager.LinkResult
				if json.Unmarshal(out.Bytes(), &result) != nil || result.URL == "" {
					t.Fatal(out.String())
				}
			}
		})
	}
}

type closedLinkOutput struct{}

func (closedLinkOutput) Write([]byte) (int, error) { return 0, errors.New("stdout unavailable") }

func TestLinkShellCopyDoesNotRequireStdout(t *testing.T) {
	copied := false
	root := newLinkCommand(CommandOptions{NewManager: func(*cobra.Command) (*manager.Manager, error) {
		return manager.New(manager.Options{ConfigFile: configFixture(t)})
	}}, linkActions{
		resolve: func(context.Context, *manager.Manager, string) (manager.LinkResult, error) {
			return manager.LinkResult{SchemaVersion: 1, URL: "https://tenant.feishu.cn/file/file"}, nil
		},
		copy: func(string) error { copied = true; return nil },
		feedback: func(_ context.Context, message string, failed bool) {
			if failed {
				t.Error("successful shell copy reported as failed", message)
			}
		},
	})
	root.SetOut(closedLinkOutput{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--path", "/selected/file", "--copy"})
	if err := root.Execute(); err != nil || !copied {
		t.Fatalf("copied=%v err=%v", copied, err)
	}
}

func TestLinkCopyFailureDoesNotReportSuccess(t *testing.T) {
	ctx := context.Background()
	feedback := ""
	_, err := finishLink(ctx, manager.LinkResult{URL: "https://tenant.feishu.cn/file/file"}, true, linkActions{
		copy: func(string) error { return errors.New("clipboard busy") },
		feedback: func(_ context.Context, s string, failed bool) {
			if !failed {
				t.Fatal("reported success")
			}
			feedback = s
		},
	})
	if err == nil || feedback == "" {
		t.Fatalf("%q %v", feedback, err)
	}
}

func TestLinkClipboardCapturedBeforeManagerAndFreshEachInvocation(t *testing.T) {
	clipboardText := "original"
	version, captures := 1, 0
	root := newLinkCommand(CommandOptions{NewManager: func(*cobra.Command) (*manager.Manager, error) {
		if captures == 0 {
			t.Fatal("clipboard was not captured before manager setup")
		}
		clipboardText = "copied while manager started"
		version++
		return manager.New(manager.Options{ConfigFile: configFixture(t)})
	}}, linkActions{
		prepareCopy: func(context.Context) (func(string) error, error) {
			captures++
			expected := version
			return func(s string) error {
				if expected != version {
					return platform.ErrLinkClipboardChanged
				}
				clipboardText = s
				return nil
			}, nil
		},
		resolve: func(context.Context, *manager.Manager, string) (manager.LinkResult, error) {
			return manager.LinkResult{URL: "https://tenant.feishu.cn/file/file"}, nil
		},
		feedback: func(_ context.Context, message string, failed bool) {
			if !failed || !strings.Contains(message, "剪贴板已变化") {
				t.Error(message, failed)
			}
		},
	})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--path", "/selected/file", "--copy"})
	for i := 1; i <= 2; i++ {
		if err := root.Execute(); !errors.Is(err, errLinkClipboard) {
			t.Fatal(err)
		}
		if captures != i || clipboardText != "copied while manager started" {
			t.Fatal(captures, clipboardText)
		}
		// Cobra retains parsed flag values when a command is executed again.
		root.SetArgs([]string{})
	}
}

func TestLinkClipboardCaptureFailureStopsBeforeManager(t *testing.T) {
	feedbackCount := 0
	root := newLinkCommand(CommandOptions{NewManager: func(*cobra.Command) (*manager.Manager, error) {
		t.Fatal("initialized manager after clipboard capture failed")
		return nil, nil
	}}, linkActions{
		prepareCopy: func(context.Context) (func(string) error, error) { return nil, errors.New("no clipboard access") },
		feedback: func(_ context.Context, message string, failed bool) {
			feedbackCount++
			if !failed || !strings.Contains(message, "剪贴板") {
				t.Error(message, failed)
			}
		},
	})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--path", "/selected/file", "--copy"})
	if err := root.Execute(); !errors.Is(err, errLinkClipboard) || feedbackCount != 1 {
		t.Fatal(err, feedbackCount)
	}
}
