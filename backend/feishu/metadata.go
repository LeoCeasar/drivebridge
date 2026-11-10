package feishu

import (
	"context"
	"errors"
	"strconv"
	"time"
)

func (item driveItem) modTime() time.Time {
	for _, v := range []string{item.ModifiedTime, item.CreatedTime} {
		if v != "" {
			seconds, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				return time.Unix(seconds, 0)
			}
		}
	}
	return time.Time{}
}
func (o *Object) materialize(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.observation != nil {
		return nil
	}
	obs, err := o.fs.cli.drive.ObserveFile(ctx, o.token)
	if err != nil {
		return err
	}
	o.observation = obs
	o.size = obs.Size
	o.fs.mu.Lock()
	o.fs.observations = append(o.fs.observations, obs)
	o.fs.mu.Unlock()
	return nil
}

// Shutdown releases private direct-mode observations. They are temporary reads,
// not the product's durable snapshots or an upload queue.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	observations := f.observations
	f.observations = nil
	f.mu.Unlock()
	var errs []error
	for _, obs := range observations {
		errs = append(errs, obs.Close())
	}
	return errors.Join(errs...)
}
