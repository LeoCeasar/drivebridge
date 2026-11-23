package syncer

import (
	"context"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
)

func TestPauseDistinguishesLifecycleInterruptionFromUserOrUnknownCancel(t *testing.T) {
	for _, tc := range []struct {
		origin, code string
		state        model.Status
	}{
		{"runtime_shutdown", "runtime_interrupted", model.StatusPaused},
		{"caller_gone", "caller_interrupted", model.StatusPaused},
		{"user_stop", "canceled", model.StatusBlocked},
		{"unspecified_cancel", "canceled", model.StatusBlocked},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			s, e, _, en := setup(t, model.KindFile)
			_, job, err := s.Stage(bg, en.ID, nil, strings.NewReader("new content"))
			if err != nil {
				t.Fatal(err)
			}
			job, _, err = s.Prepare(bg, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(online.WithCancelOrigin(bg, tc.origin))
			cancel()
			_ = e.pause(ctx, job, ctx.Err())
			journal, err := s.Journal(bg, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if journal.Job.State != tc.state || journal.ErrorCode != tc.code {
				t.Fatalf("origin=%s journal=%+v", tc.origin, journal)
			}
			if tc.state == model.StatusPaused && !transient(journal.ErrorCode) {
				t.Fatalf("safe lifecycle restart cannot resume code=%s", journal.ErrorCode)
			}
		})
	}
}
