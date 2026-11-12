package history_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func fixture(t *testing.T) (*store.Store, history.Options, model.Entry) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	sc := store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "test-user", FolderToken: "root"}
	if err = s.BindScope(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureEntry(context.Background(), "notes.md", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	return s, history.Options{GitExecutable: os.Getenv("DRIVEBRIDGE_TEST_GIT"), Dir: filepath.Join(dir, "history"), ProfileID: sc.ProfileID, IdentityHash: sc.IdentityHash}, e
}
func TestArchiveReadsAndDiffsWithoutWorkingCopies(t *testing.T) {
	ctx := context.Background()
	s, o, e := fixture(t)
	for _, body := range []string{"first\n", "second\n"} {
		if _, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Ingest(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if result.Archived != 3 {
		t.Fatalf("archive count: %+v", result)
	}
	page, err := r.List(ctx, history.Query{Kind: "local_saved"})
	if err != nil || len(page.Versions) != 2 {
		t.Fatalf("list: %+v %v", page, err)
	}
	body, err := r.Read(ctx, page.Versions[1].ID, "local")
	if err != nil || string(body) != "first\n" {
		t.Fatalf("old body %q %v", body, err)
	}
	diff, err := r.Diff(ctx, page.Versions[1].ID, page.Versions[0].ID)
	if err != nil || !strings.Contains(diff.Patch, "-first") || !strings.Contains(diff.Patch, "+second") {
		t.Fatalf("diff %+v %v", diff, err)
	}
	again, err := r.Ingest(ctx, s)
	if err != nil || again.Archived != 0 {
		t.Fatalf("duplicate: %+v %v", again, err)
	}
	reopened, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := reopened.List(ctx, history.Query{})
	if err != nil || len(page2.Versions) != 3 {
		t.Fatalf("reopen %+v %v", page2, err)
	}
	_, ack, err := s.HistoryCursor(ctx)
	if err != nil || ack != 3 {
		t.Fatalf("receipt %d %v", ack, err)
	}
}
func TestArchiveRejectsDifferentAccountAndDistinguishesEmpty(t *testing.T) {
	ctx := context.Background()
	s, o, e := fixture(t)
	_, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	r, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Ingest(ctx, s); err != nil {
		t.Fatal(err)
	}
	p, _ := r.List(ctx, history.Query{})
	b, err := r.Read(ctx, p.Versions[0].ID, "local")
	if err != nil || len(b) != 0 {
		t.Fatalf("empty %q %v", b, err)
	}
	_, err = r.Read(ctx, p.Versions[1].ID, "local")
	if !errors.Is(err, history.ErrBodyUnavailable) {
		t.Fatalf("missing %v", err)
	}
	o.IdentityHash = "someone-else"
	if _, err = history.Open(ctx, o); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("wrong account %v", err)
	}
	stream, err := r.OpenBody(ctx, p.Versions[0].ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got, _ := io.ReadAll(stream)
	if len(got) != 0 {
		t.Fatal(got)
	}
}

// A Git commit can succeed before the source receipt is durable. Replaying that
// input must acknowledge it without creating another version.
type failingReceipt struct {
	*store.Store
	fail bool
}

func (s *failingReceipt) AckHistory(ctx context.Context, seq uint64, id string) error {
	if s.fail {
		s.fail = false
		return errors.New("receipt interrupted")
	}
	return s.Store.AckHistory(ctx, seq, id)
}
func TestReplayAfterReceiptFailureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, o, e := fixture(t)
	_, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("durable"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	source := &failingReceipt{Store: s, fail: true}
	if _, err = r.Ingest(ctx, source); err == nil {
		t.Fatal("receipt fault ignored")
	}
	if _, err = r.Ingest(ctx, source); err != nil {
		t.Fatal(err)
	}
	p, err := r.List(ctx, history.Query{})
	if err != nil || len(p.Versions) != 2 {
		t.Fatalf("replay duplicated history: %+v %v", p, err)
	}
}

func TestArchiveRejectsLostOrRewoundHead(t *testing.T) {
	for _, rewind := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "rewound"}[rewind], func(t *testing.T) {
			ctx := context.Background()
			s, o, e := fixture(t)
			for _, text := range []string{"first", "second"} {
				if _, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader(text)); err != nil {
					t.Fatal(err)
				}
			}
			r, err := history.Open(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.Ingest(ctx, s); err != nil {
				t.Fatal(err)
			}
			git := o.GitExecutable
			if git == "" {
				git, err = exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--git-dir=" + filepath.Join(o.Dir, "history.git"), "update-ref"}
			if rewind {
				page, err := r.List(ctx, history.Query{})
				if err != nil || len(page.Versions) < 2 {
					t.Fatalf("history fixture: %+v %v", page, err)
				}
				args = append(args, "refs/heads/history", page.Versions[len(page.Versions)-1].Commit)
			} else {
				args = append(args, "-d", "refs/heads/history")
			}
			if output, err := exec.CommandContext(ctx, git, args...).CombinedOutput(); err != nil {
				t.Fatalf("inject ref fault: %s %v", output, err)
			}
			if _, err := history.Open(ctx, o); !errors.Is(err, history.ErrIntegrity) {
				t.Fatalf("damaged history silently accepted: %v", err)
			}
		})
	}
}
func TestArchiveBinaryCanStreamAndDetectTampering(t *testing.T) {
	ctx := context.Background()
	s, o, _ := fixture(t)
	e, err := s.EnsureEntry(ctx, "large.bin", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("\x00\x01", 1024)
	_, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	r, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Ingest(ctx, s); err != nil {
		t.Fatal(err)
	}
	p, _ := r.List(ctx, history.Query{EntryID: e.ID, Kind: "local_saved"})
	if len(p.Versions) != 1 {
		t.Fatal(p)
	}
	v := p.Versions[0]
	f, err := r.OpenBody(ctx, v.ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(got) != payload {
		t.Fatalf("binary %d %v", len(got), err)
	}
	ref := v.Bodies["local"]
	if err = os.WriteFile(filepath.Join(o.Dir, "objects", ref.Asset), []byte(strings.Repeat("!", len(payload))), 0600); err != nil {
		t.Fatal(err)
	}
	f, err = r.OpenBody(ctx, v.ID, "local")
	if err == nil {
		_, err = io.ReadAll(f)
		f.Close()
	}
	if !errors.Is(err, history.ErrIntegrity) {
		t.Fatalf("tampering accepted %v", err)
	}
}
