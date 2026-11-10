package cloud

import (
	"context"
	"errors"
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
