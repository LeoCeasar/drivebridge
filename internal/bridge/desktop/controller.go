package desktop

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

type Backend interface {
	Mounts(context.Context) ([]Mount, error)
	Status(context.Context, model.MountID) (model.MountStatus, error)
	Start(context.Context, model.MountID, bool) error
	Stop(context.Context, model.MountID) error
	Retry(context.Context, model.MountID) error
	OpenManagement(context.Context) error
	OpenFolder(context.Context, model.MountID) error
	SetAutostart(context.Context, bool) error
	Autostart(context.Context) (bool, error)
}
type ControllerOptions struct {
	Now       func() time.Time
	ErrorCode func(error) string
}
type recovery struct {
	attempts int
	next     time.Time
	code     string
	active   bool
}
type Controller struct {
	ctx       context.Context
	b         Backend
	o         ControllerOptions
	mu        sync.Mutex
	recovery  map[model.MountID]recovery
	busy      int
	lastError string
	wg        sync.WaitGroup
}

func NewController(ctx context.Context, b Backend, o ControllerOptions) *Controller {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ErrorCode == nil {
		o.ErrorCode = func(err error) string {
			if err == nil {
				return ""
			}
			return "operation_failed"
		}
	}
	return &Controller{ctx: ctx, b: b, o: o, recovery: map[model.MountID]recovery{}}
}
func (c *Controller) Wait() { c.wg.Wait() }
func (c *Controller) Poll(ctx context.Context) State {
	mounts, err := c.b.Mounts(ctx)
	if err != nil {
		return State{Kind: "error", Text: "配置暂不可用", Error: c.o.ErrorCode(err), Mounts: []MountView{}}
	}
	views := make([]MountView, 0, len(mounts))
	seen := map[model.MountID]bool{}
	for _, m := range mounts {
		seen[m.ID] = true
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := c.b.Status(readCtx, m.ID)
		cancel()
		v := MountView{Mount: m, Status: s}
		if err != nil {
			v.Error = c.o.ErrorCode(err)
		}
		if !s.Running && m.Desired && !permanent(v.Error) && !permanent(s.LastError) {
			c.recoverMount(m.ID)
		}
		c.mu.Lock()
		r := c.recovery[m.ID]
		c.mu.Unlock()
		if !s.Running && v.Error == "" && r.code != "" {
			v.Error = r.code
		}
		views = append(views, v)
	}
	out := Aggregate(views)
	out.Autostart, err = c.b.Autostart(ctx)
	if err != nil {
		out.Error = c.o.ErrorCode(err)
		out.Kind = "error"
		out.Text = "自启动状态暂不可用"
	}
	c.mu.Lock()
	for id, r := range c.recovery {
		if !seen[id] && !r.active {
			delete(c.recovery, id)
		}
	}
	out.Busy = c.busy > 0
	if c.lastError != "" {
		out.Error = c.lastError
		out.Kind = "error"
		out.Text = "操作未完成，请打开管理界面"
	}
	c.mu.Unlock()
	return out
}
func permanent(code string) bool {
	switch code {
	case "unauthorized", "identity_changed", "driver_unavailable", "config":
		return true
	}
	return false
}
func (c *Controller) recoverMount(id model.MountID) {
	if c.ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	r := c.recovery[id]
	if r.active || r.attempts >= 5 || permanent(r.code) || c.o.Now().Before(r.next) {
		c.mu.Unlock()
		return
	}
	r.active = true
	r.attempts++
	c.recovery[id] = r
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithTimeout(c.ctx, 35*time.Second)
		defer cancel()
		err := c.b.Start(ctx, id, true)
		c.mu.Lock()
		defer c.mu.Unlock()
		r := c.recovery[id]
		r.active = false
		r.code = c.o.ErrorCode(err)
		if err == nil {
			r.attempts = 0
			r.next = c.o.Now().Add(2 * time.Second)
		} else {
			delay := 2 * time.Second << min(r.attempts-1, 7)
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
			r.next = c.o.Now().Add(delay)
		}
		c.recovery[id] = r
	}()
}

// Handle is called off the native menu thread. User actions use the ordinary
// Manager path so their durable intent overrides automatic recovery.
func (c *Controller) Handle(ctx context.Context, kind string, id model.MountID) error {
	c.mu.Lock()
	c.busy++
	c.lastError = ""
	c.mu.Unlock()
	var err error
	defer func() { c.mu.Lock(); c.busy--; c.lastError = c.o.ErrorCode(err); c.mu.Unlock() }()
	switch kind {
	case "open_manager":
		err = c.b.OpenManagement(ctx)
	case "enable_autostart":
		err = c.b.SetAutostart(ctx, true)
	case "disable_autostart":
		err = c.b.SetAutostart(ctx, false)
	case "stop_all":
		var mounts []Mount
		mounts, err = c.b.Mounts(ctx)
		if err == nil {
			for _, m := range mounts {
				err = errors.Join(err, c.b.Stop(ctx, m.ID))
			}
		}
	case "start", "stop", "retry", "open_folder":
		var mounts []Mount
		mounts, err = c.b.Mounts(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, m := range mounts {
			if m.ID == id {
				found = true
				break
			}
		}
		if !found {
			err = errors.New("unknown configured mount")
			return err
		}
		if kind == "start" || kind == "retry" {
			c.mu.Lock()
			delete(c.recovery, id)
			c.mu.Unlock()
		}
		switch kind {
		case "start":
			err = c.b.Start(ctx, id, false)
		case "stop":
			err = c.b.Stop(ctx, id)
		case "retry":
			err = c.b.Retry(ctx, id)
		case "open_folder":
			err = c.b.OpenFolder(ctx, id)
		}
	default:
		err = errors.New("unsupported desktop action")
	}
	return err
}
