package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
)

type liveCase struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Result string `json:"result"`
}

// TestLiveFeishuFileObservations is a deliberately narrower live helper, not the
// complete LV-01..05/native release matrix. Only the parent/operator may opt in.
func TestLiveFeishuFileObservations(t *testing.T) {
	optIn(t, "DRIVEBRIDGE_LIVE", "DRIVEBRIDGE_REQUIRE_LIVE")
	if os.Getenv("DRIVEBRIDGE_LIVE_ALLOW_MUTATIONS") != "1" || os.Getenv("DRIVEBRIDGE_LIVE_CONFIRM_ISOLATED") != "1" {
		t.Fatal("explicit mutation and isolated-parent-folder confirmations required")
	}
	cfg, exe, profileName, parent := os.Getenv("DRIVEBRIDGE_LIVE_CONFIG"), os.Getenv("DRIVEBRIDGE_LIVE_CLI"), os.Getenv("DRIVEBRIDGE_LIVE_PROFILE"), os.Getenv("DRIVEBRIDGE_LIVE_FOLDER_TOKEN")
	if !filepath.IsAbs(cfg) || !filepath.IsAbs(exe) || profileName == "" || parent == "" {
		t.Fatal("absolute config/CLI paths plus profile and isolated folder resource reference are required")
	}
	if os.Getenv("DRIVEBRIDGE_REQUIRE_LIVE_FULL") == "1" {
		t.Fatal("full LV-01..05 acceptance is not implemented by this file/doc observation helper; run the separate complete release acceptance plan")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m, err := manager.New(manager.Options{ConfigFile: cfg, CLIExecutable: exe})
	if err != nil {
		t.Fatalf("live manager unavailable (%T)", err)
	}
	defer m.Close()
	profile, err := m.ResolveProfile(ctx, profileName)
	if err != nil {
		t.Fatalf("explicit live profile unavailable (%T)", err)
	}
	authService, err := m.Auth(ctx)
	if err != nil {
		t.Fatalf("managed auth metadata unavailable (%T)", err)
	}
	binding, err := authService.Resolve(ctx, profile.ID)
	if err != nil {
		t.Fatalf("verified identity binding unavailable (%T)", err)
	}
	runner, err := authService.ManagedRunner(ctx, profile.ID)
	if err != nil {
		t.Fatalf("managed typed runner unavailable (%T)", err)
	}
	client, err := cloud.New(runner, cloud.Options{TempDir: t.TempDir(), CheckIdentity: func(ctx context.Context) error {
		if _, err := authService.Status(ctx, profile.ID); err != nil {
			return err
		}
		now, err := authService.Resolve(ctx, profile.ID)
		if err != nil {
			return err
		}
		if now.IdentityHash != binding.IdentityHash {
			return model.ErrIdentityChanged
		}
		return nil
	}})
	if err != nil {
		t.Fatal("managed cloud construction failed")
	}
	if _, err = client.List(ctx, parent); err != nil {
		t.Fatalf("isolated parent is not readable (%T)", err)
	}
	nonce := uuid.NewString()
	runName := "DriveBridge integration " + nonce
	var folder string
	var folderVerified bool
	type owned struct {
		token, name string
		kind        model.Kind
	}
	var ownedResources []owned
	var cases []liveCase
	defer func() {
		cleanupResult := "unverified_run_folder_retained"
		if folderVerified {
			cleanupResult = "retained_by_request"
		}
		if folderVerified && os.Getenv("DRIVEBRIDGE_LIVE_KEEP") != "1" {
			cleanupResult = "cleaned_verified_owned_resources"
			for _, resource := range ownedResources {
				items, err := client.List(ctx, folder)
				if err != nil {
					cleanupResult = "cleanup_incomplete"
					continue
				}
				match := false
				for _, item := range items {
					if item.Token == resource.token && item.Name == resource.name {
						match = true
					}
				}
				if match {
					if err = client.Delete(ctx, resource.token, resource.kind); err != nil {
						cleanupResult = "cleanup_incomplete"
					}
				}
			}
			remaining, err := client.List(ctx, folder)
			if err == nil && len(remaining) == 0 {
				if err = client.Delete(ctx, folder, model.KindFolder); err != nil {
					cleanupResult = "cleanup_incomplete"
				}
			} else {
				cleanupResult = "retained_nonempty_or_unknown_run_folder"
			}
		}
		writeEvidence(t, "DRIVEBRIDGE_LIVE_EVIDENCE_DIR", "live-"+nonce+".json", map[string]any{"schema_version": 1, "coverage": "typed file observations and newly created document projection only; not full release acceptance", "goos": runtime.GOOS, "goarch": runtime.GOARCH, "profile_id": profile.ID, "run_folder_name": runName, "run_folder_resource": folder, "cases": cases, "cleanup": cleanupResult, "completed_at": time.Now().UTC()})
	}()
	folder, err = client.CreateFolder(ctx, parent, runName)
	if err != nil {
		t.Fatalf("isolated run folder create failed; no retry issued (%T)", err)
	}
	listed, err := client.List(ctx, parent)
	if err != nil {
		t.Fatal("cannot verify created folder identity; left for operator recovery")
	}
	found := false
	for _, item := range listed {
		if item.Token == folder && item.Name == runName && item.Type == "folder" {
			found = true
		}
	}
	if !found {
		t.Fatal("created folder identity not independently listed; no cleanup attempted")
	}
	folderVerified = true
	for _, item := range []struct {
		name string
		data []byte
	}{{"small", []byte("DriveBridge live observation " + nonce + "\n")}, {"large-16MiB", bytes.Repeat([]byte("0123456789abcdef"), 1<<20)}, {"empty", []byte{}}} {
		name := item.name + "-" + nonce + ".bin"
		index := len(cases)
		cases = append(cases, liveCase{Name: name, Size: int64(len(item.data)), Result: "create_maybe_unknown"})
		uploaded, uploadErr := client.UploadFile(ctx, folder, "", name, bytes.NewReader(item.data))
		if uploadErr != nil {
			t.Fatalf("file upload unconfirmed; ambiguous result never retried, named fixture retained in evidence (%T)", uploadErr)
		}
		items, err := client.List(ctx, folder)
		if err != nil {
			t.Fatal("cannot verify created file location")
		}
		verified := false
		for _, row := range items {
			if row.Token == uploaded.FileToken && row.Name == name {
				verified = true
			}
		}
		if !verified {
			t.Fatal("uploaded resource is not independently listed under the isolated folder")
		}
		ownedResources = append(ownedResources, owned{uploaded.FileToken, name, model.KindFile})
		observation, err := client.ObserveFile(ctx, uploaded.FileToken)
		if err != nil {
			t.Fatalf("actual uploaded bytes could not be observed (%T)", err)
		}
		reader, err := observation.Open()
		if err != nil {
			observation.Close()
			t.Fatal("observation lease open failed")
		}
		hash := sha256.New()
		n, readErr := io.Copy(hash, reader)
		closeErr := errors.Join(reader.Close(), observation.Close())
		expected := sha256.Sum256(item.data)
		actual := hex.EncodeToString(hash.Sum(nil))
		if readErr != nil || closeErr != nil || n != int64(len(item.data)) || observation.Size != n || actual != hex.EncodeToString(expected[:]) || observation.Identity.SHA256 != actual {
			t.Fatal("independent live bytes/count/SHA differ")
		}
		if err = observation.Identity.Validate(); err != nil {
			t.Fatal("invalid live observation identity")
		}
		cases[index] = liveCase{Name: name, Size: n, SHA256: actual, Result: "independent_whole_download_matches_input"}
	}
	docName := "document-" + nonce
	docIndex := len(cases)
	cases = append(cases, liveCase{Name: docName, Result: "create_maybe_unknown"})
	created, err := client.CreateDoc(ctx, folder, docName, []byte("<p>DriveBridge live document "+nonce+"</p>"))
	if err != nil {
		t.Fatalf("document create result requires operator reconciliation; no retry (%T)", err)
	}
	docItems, err := client.List(ctx, folder)
	if err != nil {
		t.Fatal("cannot verify created document location")
	}
	docFound := false
	for _, item := range docItems {
		if item.Token == created.Token && item.Name == docName && item.Type == "docx" {
			docFound = true
		}
	}
	if !docFound {
		t.Fatal("document identity not independently listed")
	}
	ownedResources = append(ownedResources, owned{created.Token, docName, model.KindDocx})
	actualDoc, err := client.FetchDoc(ctx, created.Token, "")
	if err != nil {
		t.Fatalf("document readback failed (%T)", err)
	}
	view, err := markdown.DecodeWithOptions(actualDoc.XML, actualDoc.Revision, markdown.Options{SourceURL: actualDoc.SourceURL, ReferenceMap: actualDoc.ReferenceMap})
	if err != nil || !strings.Contains(string(view.Markdown), nonce) {
		t.Fatal("created document projection did not preserve the independent input")
	}
	cases[docIndex] = liveCase{Name: docName, Size: int64(len(view.Markdown)), Result: "observed_input_in_body"}
}
func TestLiveFeishuFullAcceptanceGate(t *testing.T) {
	if os.Getenv("DRIVEBRIDGE_REQUIRE_LIVE_FULL") == "1" {
		t.Fatal("full live gate remains unmet: multi-account credential lifecycle, permission revocation, verified per-operation partial patches, real multi-page listing, history unavailability and two-client concurrency require the documented LV-01..05 operator plan")
	}
	t.Skip("UNVERIFIED: full LV-01..05 live acceptance is separate; this skip is never release evidence")
}
