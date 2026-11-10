package cloud

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestResourceKindsRejectedBeforeRunner(t *testing.T) {
	c, err := New(managed(t, "must-not-run"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []model.Kind{"", "wiki", "unexpected"} {
		if err = c.Rename(context.Background(), "token", kind, "name"); !errors.Is(err, model.ErrUnsupportedContent) {
			t.Fatal(err)
		}
		if err = c.Move(context.Background(), "token", kind, "parent"); !errors.Is(err, model.ErrUnsupportedContent) {
			t.Fatal(err)
		}
		if err = c.Delete(context.Background(), "token", kind); !errors.Is(err, model.ErrUnsupportedContent) {
			t.Fatal(err)
		}
	}
}

func TestIdentityPreflightBlocksRPCBeforeDispatch(t *testing.T) {
	c, err := New(managed(t, "must-not-run"), Options{CheckIdentity: func(context.Context) error { return model.ErrIdentityChanged }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(context.Background(), "folder"); !errors.Is(err, ErrNotDispatched) || !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	if err = c.Rename(context.Background(), "token", model.KindDocx, "title"); !errors.Is(err, ErrNotDispatched) {
		t.Fatal(err)
	}
	if _, err = c.CreateDoc(context.Background(), "folder", "title", nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatal(err)
	}
}

func TestMutationGateLeavesReadCapabilitiesAvailable(t *testing.T) {
	c, err := New(managed(t, "must-not-run"), Options{CheckMutation: func(context.Context) error { return model.ErrUnsupportedContent }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.FileMeta(context.Background(), "token"); errors.Is(err, ErrNotDispatched) {
		t.Fatal("read route blocked by mutation capability")
	}
	if _, err = c.UploadFile(context.Background(), "folder", "token", "name", strings.NewReader("bytes")); !errors.Is(err, ErrNotDispatched) || !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	if err = c.Delete(context.Background(), "token", model.KindFile); !errors.Is(err, ErrNotDispatched) {
		t.Fatal(err)
	}
}
