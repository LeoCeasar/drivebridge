package larkcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func arg(args []string, key string) string {
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func handleDriveProcess(s string, args []string, emit func(string)) bool {
	if s == "upload-response" {
		emit(arg(args, "--name"))
		return true
	}
	if s == "upload-canceled-response" {
		emit(`{"file_token":"candidate","size":7,"url":"https://example.invalid/candidate","version":"3"}`)
		time.Sleep(time.Minute)
		return true
	}
	if s == "upload-failed-response" {
		emit(`{"file_token":"candidate","size":7,"url":"https://example.invalid/candidate","version":"3"}`)
		os.Exit(1)
	}
	if s == "drive-routes" {
		switch {
		case len(args) > 0 && args[0] == "api":
			if args[2] != "/open-apis/drive/v1/files/file/history" || arg(args, "--params") != `{"only_tag":false,"page_size":200}` {
				os.Exit(2)
			}
			emit(`{"items":[{"version":"1","size":"45","edit_time":2,"is_deleted":false,"type":2},{"version":"2","size":81,"edit_time":1,"is_deleted":false,"type":1}],"has_more":false}`)
		case len(args) > 2 && args[2] == "patch":
			b, _ := io.ReadAll(os.Stdin)
			if arg(args, "--data") != "-" || string(b) != `{"new_title":"new name"}` {
				os.Exit(2)
			}
			emit(`{}`)
		case len(args) > 1 && args[1] == "+upload":
			p := arg(args, "--file")
			if filepath.IsAbs(p) || p == "" {
				os.Exit(2)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				os.Exit(2)
			}
			emit(fmt.Sprintf(`{"file_token":"file","size":%d}`, len(b)))
		default:
			os.Exit(2)
		}
		return true
	}

	if strings.HasPrefix(s, "list") {
		if arg(args, "--as") != "user" || arg(args, "--page-all") != "" {
			os.Exit(2)
		}
		next := arg(args, "--page-token")
		if next == "" {
			emit(`{"files":[{"token":"f1","name":"one","type":"file"}],"has_more":true,"next_page_token":"opaque"}`)
			return true
		}
		switch s {
		case "list-fail":
			fmt.Fprint(os.Stderr, `{"ok":false,"identity":"user","error":{"type":"api","code":400}}`)
			os.Exit(1)
		case "list-repeat":
			emit(`{"files":[],"has_more":true,"next_page_token":"opaque"}`)
		case "list-missing":
			emit(`{"files":[]}`)
		case "list-disconnect":
			os.Exit(1)
		case "list-cancel":
			time.Sleep(time.Minute)
		case "list-duplicate":
			emit(`{"files":[{"token":"f1","name":"renamed","type":"file"}],"has_more":false}`)
		default:
			var items []map[string]string
			for i := 0; i < 3000; i++ {
				items = append(items, map[string]string{"token": fmt.Sprint(i), "name": strings.Repeat("a", 30), "type": "file"})
			}
			data, _ := json.Marshal(map[string]any{"files": items, "has_more": false})
			emit(string(data))
		}
		return true
	}
	if strings.HasPrefix(s, "download") {
		if filepath.IsAbs(arg(args, "--output")) || arg(args, "--version") != "" || arg(args, "--file-token") != "file" {
			os.Exit(2)
		}
		name := "download"
		body := []byte("content")
		if s == "download-empty" {
			body = nil
		}
		os.WriteFile(name, body, 0600)
		size := len(body)
		switch s {
		case "download-mismatch":
			size++
		case "download-path":
			name = "../escape"
		case "download-symlink":
			os.Remove(name)
			os.Symlink(os.DevNull, name)
		case "download-fail":
			os.Exit(1)
		case "download-cancel":
			time.Sleep(time.Minute)
		case "download-missing":
			emit(`{"saved_path":"download"}`)
			return true
		}
		data, _ := json.Marshal(map[string]any{"saved_path": name, "size_bytes": size})
		emit(string(data))
		if s == "download-nonzero-success" {
			os.Exit(1)
		}
		return true
	}
	return false
}
func TestPagedJSONNoTruncation(t *testing.T) {
	d := NewDrive(NewDirect(testExecutable(t, "list")), "")
	items, err := d.List(context.Background(), "folder")
	if err != nil || len(items) != 3001 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
}
func TestPaginationFailuresReturnNoPartialSnapshot(t *testing.T) {
	for _, s := range []string{"list-fail", "list-repeat", "list-missing", "list-disconnect", "list-duplicate", "list-cancel"} {
		t.Run(s, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			items, err := NewDrive(NewDirect(testExecutable(t, s)), "").List(ctx, "folder")
			if err == nil || items != nil {
				t.Fatalf("%d %v", len(items), err)
			}
		})
	}
}
func TestMetadataPresenceAndCurrentVersion(t *testing.T) {
	for _, v := range []string{`0`, `"0"`, `81`, `"9007199254740993"`} {
		page, err := DecodeHistory([]byte(`{"items":[{"version":"7633658129540910621","size":` + v + `,"edit_time":1777013761763,"is_deleted":false,"type":1}],"has_more":false}`))
		if err != nil || len(page.Items) != 1 {
			t.Fatal(v, err)
		}
	}
	for _, v := range []string{`null`, `-1`, `1.1`, `1e3`, `"9223372036854775808"`, `true`, `{}`} {
		_, err := DecodeHistory([]byte(`{"items":[{"version":"123","size":` + v + `,"edit_time":1,"is_deleted":false}],"has_more":false}`))
		if err == nil {
			t.Fatal("accepted", v)
		}
	}
	for _, fixture := range []string{`{"items":[{"version":"123","edit_time":1,"is_deleted":false}],"has_more":false}`, `{"items":[{"version":"123","size":0,"edit_time":1}],"has_more":false}`} {
		if _, err := DecodeHistory([]byte(fixture)); err == nil {
			t.Fatal("missing accepted")
		}
	}
	_, err := NewDrive(NewDirect(testExecutable(t, "default")), "").FileMeta(context.Background(), "token")
	if !errors.Is(err, ErrMetadataUnknown) {
		t.Fatalf("%v", err)
	}
}
func TestObservationOwnsVerifiedBytes(t *testing.T) {
	for _, s := range []string{"download", "download-empty"} {
		t.Run(s, func(t *testing.T) {
			d := NewDrive(NewDirect(testExecutable(t, s)), t.TempDir())
			o, err := d.ObserveFile(context.Background(), "file")
			if err != nil {
				t.Fatal(err)
			}
			rc, err := o.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			sum := sha256.Sum256(b)
			if o.Size != int64(len(b)) || o.SHA256 != hex.EncodeToString(sum[:]) || o.Token != "file" || o.ObservationID == "" || o.StartedAt.IsZero() || o.CompletedAt.Before(o.StartedAt) {
				t.Fatalf("%+v", o)
			}
			if err = o.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = o.Open(); err == nil {
				t.Fatal("closed lease opened")
			}
			if err = o.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestObservationRejectsUnadoptableDownload(t *testing.T) {
	for _, s := range []string{"download-mismatch", "download-path", "download-symlink", "download-fail", "download-cancel", "download-missing", "download-nonzero-success"} {
		t.Run(s, func(t *testing.T) {
			base := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			o, err := NewDrive(NewDirect(testExecutable(t, s)), base).ObserveFile(ctx, "file")
			if err == nil || o != nil {
				t.Fatal("adopted failed download")
			}
			files, _ := os.ReadDir(base)
			if len(files) != 0 {
				t.Fatal("temp file leaked")
			}
		})
	}
}

func TestObservationReaderSurvivesLeaseRelease(t *testing.T) {
	base := t.TempDir()
	o, err := NewDrive(NewDirect(testExecutable(t, "download")), base).ObserveFile(context.Background(), "file")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := o.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	held, _ := os.ReadDir(base)
	if len(held) != 1 {
		t.Fatal("lease removed before reader release")
	}
	b, err := io.ReadAll(rc)
	if err != nil || string(b) != "content" {
		t.Fatal(string(b), err)
	}
	if err = rc.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(base)
	if len(files) != 0 {
		t.Fatal("released reader left private files")
	}
}
func TestRawHistoryAndDriveMutationArgv(t *testing.T) {
	d := NewDrive(NewDirect(testExecutable(t, "drive-routes")), t.TempDir())
	page, err := d.HistoryPage(context.Background(), "file", "")
	if err != nil || len(page.Items) != 2 || page.Items[0].Version != "1" || page.Items[1].Version != "2" {
		t.Fatal(page, err)
	}
	if err = d.Rename(context.Background(), "file", "file", "new name"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"content", ""} {
		u, err := d.Upload(context.Background(), "parent", "", "name", strings.NewReader(body))
		if err != nil || u.Size != int64(len(body)) || u.FileToken != "file" {
			t.Fatal(u, err)
		}
	}
}

func TestPageRequiredFieldTypes(t *testing.T) {
	for _, body := range []string{`{"files":null,"has_more":false}`, `{"files":{},"has_more":false}`, `{"files":[],"has_more":null}`, `{"files":[],"has_more":"false"}`, `{"files":[],"has_more":true}`, `{"files":[],"has_more":true,"next_page_token":3}`, `{"files":[],"has_more":false,"next_page_token":"unexpected"}`, `{"files":[{"token":"x","name":7,"type":"file"}],"has_more":false}`} {
		if _, err := decodePage([]byte(body), "folder"); err == nil {
			t.Fatal("incomplete page accepted", body)
		}
	}
}

type uploadReadFailure struct{ err error }

func (r uploadReadFailure) Read([]byte) (int, error) { return 0, r.err }

func TestUploadStagingFailureIsNotDispatched(t *testing.T) {
	readErr := errors.New("source read failed")
	for _, tc := range []struct {
		name, tempDir string
		in            io.Reader
		wantErr       error
	}{
		{"missing-temp-directory", filepath.Join(t.TempDir(), "missing"), strings.NewReader("content"), os.ErrNotExist},
		{"reader-failure", t.TempDir(), uploadReadFailure{readErr}, readErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil runner makes any attempt to dispatch fail the test immediately.
			result, err := NewDrive(nil, tc.tempDir).Upload(context.Background(), "parent", "", "name", tc.in)
			if !errors.Is(err, tc.wantErr) || !errors.Is(err, ErrNotDispatched) || errors.Is(err, ErrUnknownResult) || result != (UploadResult{}) {
				t.Fatalf("staging error/result classification: %+v %v", result, err)
			}
		})
	}
}

func TestUploadPreservesCandidateOnProtocolFailure(t *testing.T) {
	executable := testExecutable(t, "upload-response")
	for _, tc := range []struct {
		name, data, boundToken string
		want                   UploadResult
	}{
		{"bad-size", `{"file_token":"candidate","size":"7","url":"https://example.invalid/candidate","version":"3"}`, "", UploadResult{FileToken: "candidate", URL: "https://example.invalid/candidate", Version: "3"}},
		{"size-mismatch", `{"file_token":"candidate","size":8}`, "", UploadResult{FileToken: "candidate", Size: 8}},
		{"missing-size", `{"file_token":"candidate","url":"https://example.invalid/candidate"}`, "", UploadResult{FileToken: "candidate", URL: "https://example.invalid/candidate"}},
		{"bad-url", `{"file_token":"candidate","size":7,"url":7,"version":"3"}`, "", UploadResult{FileToken: "candidate", Size: 7, Version: "3"}},
		{"bad-version", `{"file_token":"candidate","size":7,"version":3}`, "", UploadResult{FileToken: "candidate", Size: 7}},
		{"bound-token-mismatch", `{"file_token":"candidate","size":7}`, "bound", UploadResult{FileToken: "candidate", Size: 7}},
		{"invalid-token", `{"file_token":7,"size":7,"version":"3"}`, "", UploadResult{Size: 7, Version: "3"}},
		{"invalid-json", `{"file_token":"candidate","size":7} trailing`, "", UploadResult{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewDrive(NewDirect(executable), t.TempDir()).Upload(context.Background(), "parent", tc.boundToken, tc.data, strings.NewReader("content"))
			if !errors.Is(err, ErrUnknownResult) || !errors.Is(err, ErrProtocol) || errors.Is(err, ErrNotDispatched) || result != tc.want {
				t.Fatalf("candidate evidence or strict success validation: %+v %v; want %+v", result, err, tc.want)
			}
		})
	}
}

func TestUploadPreservesCandidateOnRunnerFailure(t *testing.T) {
	for _, scenario := range []string{"upload-failed-response", "upload-canceled-response"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			result, err := NewDrive(NewDirect(testExecutable(t, scenario)), t.TempDir()).Upload(ctx, "parent", "", "name", strings.NewReader("content"))
			want := UploadResult{FileToken: "candidate", Size: 7, URL: "https://example.invalid/candidate", Version: "3"}
			if !errors.Is(err, ErrUnknownResult) || errors.Is(err, ErrNotDispatched) || result != want {
				t.Fatalf("runner failure discarded candidate: %+v %v", result, err)
			}
			if scenario == "upload-canceled-response" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
		})
	}
}
