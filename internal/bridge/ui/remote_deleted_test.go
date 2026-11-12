package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

type deletionBackend struct {
	*fakeBackend
	requests []model.ResolveRequest
}

func (b *deletionBackend) Resolve(_ context.Context, r model.ResolveRequest) error {
	b.requests = append(b.requests, r)
	return nil
}

func TestRemoteDeletedConflictRequiresExplicitDecision(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		text         bool
		content      []byte
		status       int
		saves        bool
	}{
		{"ordinary merge rejected", "", true, []byte("local edit"), 400, false},
		{"unknown decision rejected", "unknown", true, nil, 400, false},
		{"follow deletion keeps existing draft", model.ResolveAcceptDelete, false, nil, 200, false},
		{"binary retained without content", model.ResolveRestoreLocal, false, nil, 200, false},
		{"text restore keeps captured content", model.ResolveRestoreLocal, true, nil, 200, false},
		{"text restore includes edited draft", model.ResolveRestoreLocal, true, []byte("edited MD\r\n"), 200, true},
		{"text restore can intentionally be empty", model.ResolveRestoreLocal, true, []byte{}, 200, true},
		{"follow deletion ignores content without erasing draft", model.ResolveAcceptDelete, true, []byte("ignored"), 200, false},
		{"binary edit rejected", model.ResolveRestoreLocal, false, []byte("wrong"), 422, false},
		{"invalid UTF8 rejected", model.ResolveRestoreLocal, true, []byte{0xff}, 422, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake, c, csrf, _ := fixture(t)
			b := &deletionBackend{fakeBackend: fake}
			h.backend = b
			fake.input.Text = tc.text
			fake.input.Conflict.Reason = model.ConflictRemoteDeleted
			fake.input.Conflict.RemoteRevision = "deleted:token"
			if err := os.WriteFile(fake.draftFile, []byte("meaningful existing draft"), 0600); err != nil {
				t.Fatal(err)
			}
			id := fake.input.Conflict.ID
			res := request(t, c, h, "GET", "/api/conflicts/"+string(id), nil, "", "")
			var view struct {
				Reason        string `json:"reason"`
				RemoteDeleted bool   `json:"remote_deleted"`
			}
			err := json.NewDecoder(res.Body).Decode(&view)
			res.Body.Close()
			if err != nil || !view.RemoteDeleted || view.Reason != model.ConflictRemoteDeleted {
				t.Fatalf("view=%+v err=%v", view, err)
			}
			in := model.ResolveRequest{ConflictID: id, Action: tc.action, ExpectedGeneration: 4, ExpectedRemoteRevision: "deleted:token", Content: tc.content}
			res = request(t, c, h, "POST", "/api/conflicts/"+string(id)+"/resolve", in, csrf, h.URL())
			res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d", res.StatusCode, tc.status)
			}
			saved, err := os.ReadFile(fake.draftFile)
			if err != nil {
				t.Fatal(err)
			}
			expected := "meaningful existing draft"
			if tc.saves {
				expected = string(tc.content)
			}
			if string(saved) != expected {
				t.Fatalf("draft=%q want=%q", saved, expected)
			}
			if tc.status == http.StatusOK && (len(b.requests) != 1 || b.requests[0].Action != tc.action || (tc.content == nil && b.requests[0].Content != nil)) {
				t.Fatalf("requests=%+v", b.requests)
			}
			if tc.status != http.StatusOK && len(b.requests) != 0 {
				t.Fatal("invalid decision reached backend")
			}
		})
	}
}

func TestDeletionDecisionPreservesVersionAndStaleGuards(t *testing.T) {
	for _, reason := range []string{model.ConflictRemoteDeleted, model.ConflictLocalDeletePending} {
		for _, tc := range []struct {
			name       string
			stale      bool
			generation uint64
			revision   string
			status     int
		}{
			{"changed generation", false, 5, "r1", 409}, {"changed deletion revision", false, 4, "r2", 409}, {"stale observation", true, 4, "r1", 503},
		} {
			t.Run(reason+"/"+tc.name, func(t *testing.T) {
				h, b, c, csrf, _ := fixture(t)
				b.input.Conflict.Reason = reason
				b.input.Stale = tc.stale
				b.input.Entry.Generation = tc.generation
				b.input.Conflict.RemoteRevision = tc.revision
				in := model.ResolveRequest{ConflictID: b.input.Conflict.ID, Action: model.ResolveAcceptDelete, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1"}
				res := request(t, c, h, "POST", "/api/conflicts/"+string(in.ConflictID)+"/resolve", in, csrf, h.URL())
				res.Body.Close()
				if res.StatusCode != tc.status || b.resolves != 0 {
					t.Fatal(res.Status, b.resolves)
				}
				if _, err := os.Stat(b.draftFile); !os.IsNotExist(err) {
					t.Fatal("delete decision wrote a draft", err)
				}
			})
		}

	}
}

func TestOrdinaryConflictStillRequiresEditableUTF8(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    bool
		content []byte
	}{
		{"binary", false, []byte("contents")}, {"invalid UTF8", true, []byte{0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, b, c, csrf, _ := fixture(t)
			b.input.Text = tc.text
			in := model.ResolveRequest{ConflictID: b.input.Conflict.ID, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1", Content: tc.content}
			res := request(t, c, h, "POST", "/api/conflicts/"+string(in.ConflictID)+"/resolve", in, csrf, h.URL())
			res.Body.Close()
			if res.StatusCode != http.StatusUnprocessableEntity || b.resolves != 0 {
				t.Fatal(res.Status, b.resolves)
			}
		})
	}
}

func TestOrdinaryConflictRejectsDeletionActions(t *testing.T) {
	for _, action := range []string{model.ResolveRestoreLocal, model.ResolveAcceptDelete, "unknown"} {
		t.Run(action, func(t *testing.T) {
			h, b, c, csrf, _ := fixture(t)
			in := model.ResolveRequest{ConflictID: b.input.Conflict.ID, Action: action, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1"}
			res := request(t, c, h, "POST", "/api/conflicts/"+string(in.ConflictID)+"/resolve", in, csrf, h.URL())
			res.Body.Close()
			if res.StatusCode != 400 || b.resolves != 0 {
				t.Fatal(res.Status, b.resolves)
			}
		})
	}
}

func TestLocalDeletePendingRequiresExplicitLocalDecision(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		text         bool
		content      []byte
		status       int
		save         bool
	}{
		{"ordinary merge rejected", "", true, []byte("edited"), 400, false},
		{"unknown action rejected", "unknown", true, nil, 400, false},
		{"binary retained", model.ResolveRestoreLocal, false, nil, 200, false},
		{"edited text retained", model.ResolveRestoreLocal, true, []byte("edited pending file"), 200, true},
		{"local deletion cancels upload", model.ResolveAcceptDelete, false, nil, 200, false},
		{"binary edited content rejected", model.ResolveRestoreLocal, false, []byte("edited"), 422, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake, c, csrf, _ := fixture(t)
			b := &deletionBackend{fakeBackend: fake}
			h.backend = b
			fake.input.Conflict.Reason = model.ConflictLocalDeletePending
			fake.input.Conflict.RemoteRevision = model.LocalDeletionRevision(fake.input.Entry.ID, fake.input.Entry.Generation)
			fake.input.Text = tc.text
			if err := os.WriteFile(fake.draftFile, []byte("existing historical draft"), 0600); err != nil {
				t.Fatal(err)
			}
			id := fake.input.Conflict.ID
			res := request(t, c, h, "GET", "/api/conflicts/"+string(id), nil, "", "")
			var view struct {
				Reason             string `json:"reason"`
				RemoteDeleted      bool   `json:"remote_deleted"`
				LocalDeletePending bool   `json:"local_delete_pending"`
			}
			err := json.NewDecoder(res.Body).Decode(&view)
			res.Body.Close()
			if err != nil || !view.LocalDeletePending || view.RemoteDeleted || view.Reason != model.ConflictLocalDeletePending {
				t.Fatalf("view=%+v err=%v", view, err)
			}
			in := model.ResolveRequest{ConflictID: id, Action: tc.action, ExpectedGeneration: 4, ExpectedRemoteRevision: fake.input.Conflict.RemoteRevision, Content: tc.content}
			res = request(t, c, h, "POST", "/api/conflicts/"+string(id)+"/resolve", in, csrf, h.URL())
			res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d", res.StatusCode, tc.status)
			}
			saved, err := os.ReadFile(fake.draftFile)
			if err != nil {
				t.Fatal(err)
			}
			expected := "existing historical draft"
			if tc.save {
				expected = string(tc.content)
			}
			if string(saved) != expected {
				t.Fatalf("draft=%q want=%q", saved, expected)
			}
			if tc.status == 200 && (len(b.requests) != 1 || b.requests[0].Action != tc.action) {
				t.Fatalf("requests=%+v", b.requests)
			}
			if tc.status != 200 && len(b.requests) > 0 {
				t.Fatal("invalid request reached backend")
			}
		})
	}
}
