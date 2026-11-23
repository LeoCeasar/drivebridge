package auth

import (
	"context"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/onlinecooldown"
	"github.com/rclone/rclone/lib/larkcli"
)

func (s *Service) onlineCoordinator(binding Binding) (*onlinecooldown.Coordinator, error) {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	if s.cooldowns == nil {
		s.cooldowns = map[string]*onlinecooldown.Coordinator{}
	}
	key := binding.IdentityHash + "|" + binding.AuthRef
	if c := s.cooldowns[key]; c != nil {
		return c, nil
	}
	c, err := onlinecooldown.New(s.domain, binding.IdentityHash, binding.AuthRef, nil)
	if err == nil {
		s.cooldowns[key] = c
	}
	return c, err
}

func (s *Service) withOnlinePolicy(ctx context.Context, id model.ProfileID, binding Binding, runner *larkcli.Runner) (*larkcli.Runner, error) {
	c, err := s.onlineCoordinator(binding)
	if err != nil {
		return nil, err
	}
	return runner.WithRequestHooks(func(ctx context.Context) error {
		current, err := s.Resolve(ctx, id)
		if err != nil {
			return err
		}
		if current.IdentityHash != binding.IdentityHash || current.AuthRef != binding.AuthRef {
			return larkcli.ErrIdentity
		}
		return c.Check(ctx)
	}, func(ctx context.Context, cause error) error {
		if online.Cause(cause) != "rate_limited" {
			return nil
		}
		failure := online.Capture(ctx, cause, "sync", "unknown", "unknown", time.Now())
		delay := 2 * time.Second
		if failure.RetryAfterSeconds > 0 {
			if failure.RetryAfterSeconds > int64((1<<63-1)/int64(time.Second)) {
				delay = time.Duration(1<<63 - 1)
			} else {
				delay = time.Duration(failure.RetryAfterSeconds) * time.Second
			}
		}
		return c.Record(ctx, delay)
	}), nil
}
