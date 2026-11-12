package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/desktopstartup"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/tray"
	"github.com/rclone/rclone/internal/bridge/ui"
)

type Options struct {
	Manager       *manager.Manager
	Executable    string
	CLIExecutable string
	Headless      bool
	// Open is injectable so integration tests cannot open user browsers.
	Open func(context.Context, string) error
}

// Run must execute on the initial goroutine: native tray implementations own
// the platform UI loop. Sync workers remain independent processes.
func Run(ctx context.Context, o Options) error {
	if o.Manager == nil {
		return errors.New("desktop manager required")
	}
	if !o.Headless && !tray.Available() {
		return fmt.Errorf("%w: install a desktop build or use --headless", tray.ErrUnavailable)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	coordinator, err := auth.NewCoordinator()
	if err != nil {
		return err
	}
	initializationRelease, err := coordinator.AcquireWorkerStart(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if initializationRelease != nil {
			initializationRelease()
		}
	}()
	// Claim state/cache before ui.Start creates runtime files. A first-run
	// configuration cannot claim an already occupied, unowned private root.
	if err := o.Manager.InitializeConfig(ctx); err != nil {
		return err
	}
	open := o.Open
	if open == nil {
		open = platform.OpenBrowser
	}
	stateDir, err := platform.StateDir()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(o.Manager.ConfigFile())))
	key := hex.EncodeToString(sum[:])
	runtimeDir := filepath.Join(stateDir, "ui")
	uiBackend, err := ui.NewManagerBackend(o.Manager)
	if err != nil {
		return err
	}
	host, err := ui.Start(ctx, ui.Options{Backend: uiBackend, RuntimeDir: runtimeDir, ConfigKey: key})
	if err != nil && !errors.Is(err, ui.ErrAlreadyRunning) {
		return err
	}
	// This process closes only the host it owns, preserving an explicitly started GUI.
	if host != nil {
		defer host.Close(context.Background())
	}
	openView := func(ctx context.Context, view string) error {
		var address string
		var err error
		if host != nil {
			address, err = host.BootstrapURL("")
		} else {
			address, err = ui.OpenExisting(ctx, runtimeDir, key, "")
		}
		if err != nil {
			return err
		}
		address, err = managementViewURL(address, view)
		if err != nil {
			return err
		}
		return open(ctx, address)
	}
	openManagement := func(ctx context.Context) error { return openView(ctx, "") }
	backend := &managerBackend{manager: o.Manager, open: openManagement, openView: openView, startup: desktopstartup.Spec{Executable: o.Executable, ConfigFile: o.Manager.ConfigFile(), CLIExecutable: o.CLIExecutable}}
	controller := NewController(ctx, backend, ControllerOptions{ErrorCode: func(err error) string {
		if err == nil {
			return ""
		}
		return manager.ErrorInfo(err).Code
	}})
	updates := make(chan tray.View, 1)
	actions := make(chan tray.Action, 32)
	finished := make(chan error, 1)
	var operations sync.WaitGroup
	var release func()
	// Registration happens only after the native tray has emitted ready. A failed
	// native initialization must never satisfy `desktop start` readiness checks.
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		ready := false
		var loopErr error
		publish := func() {
			pollCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			v := trayView(controller.Poll(pollCtx))
			select {
			case updates <- v:
			default:
				select {
				case <-updates:
				default:
				}
				select {
				case updates <- v:
				default:
				}
			}
			if host != nil {
				_ = host.PollConflicts(pollCtx)
			}
		}
		defer func() { operations.Wait(); controller.Wait(); finished <- loopErr }()
		for {
			select {
			case <-ctx.Done():
				return
			case a := <-actions:
				if a.Kind == "ready" {
					if ready {
						continue
					}
					if e := o.Manager.EnableDesktop(ctx); e != nil {
						loopErr = e
						cancel()
						return
					}
					var e error
					release, e = o.Manager.DesktopRegister(ctx, openManagement, cancel)
					if e != nil {
						loopErr = e
						cancel()
						return
					}
					initializationRelease()
					initializationRelease = nil
					ready = true
					startDiscovery(ctx, o.Manager, &operations)
					startHistoryMaintenance(ctx, o.Manager, &operations)
					publish()
					mounts, e := backend.Mounts(ctx)
					if e == nil && len(mounts) == 0 && !o.Headless {
						operations.Add(1)
						go func() { defer operations.Done(); _ = openManagement(ctx) }()
					}
					continue
				}
				if !ready {
					continue
				}
				if a.Kind == "quit" {
					cancel()
					continue
				}
				operations.Add(1)
				go func() {
					defer operations.Done()
					actionCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
					defer stop()
					if a.Kind == "rescan_discovery" {
						_ = controller.HandleAccount(actionCtx, model.ProfileID(a.ProfileID))
					} else {
						_ = controller.Handle(actionCtx, a.Kind, model.MountID(a.MountID))
					}
				}()
			case <-ticker.C:
				if ready {
					publish()
				}
			}
		}
	}()
	shutdown := desktopShutdown(cancel, finished, func() error {
		if host != nil {
			return host.Close(context.Background())
		}
		return nil
	}, o.Manager.Close, func() {
		if release != nil {
			release()
		}
	})
	initial := tray.View{State: "unavailable", Text: "正在启动 DriveBridge"}
	if o.Headless {
		actions <- tray.Action{Kind: "ready"}
		<-ctx.Done()
	} else {
		err = tray.RunWithShutdown(ctx, initial, updates, actions, func() { _ = shutdown() })
		if errors.Is(err, tray.ErrNoDisplay) || errors.Is(err, tray.ErrUnavailable) {
			// Linux desktop environments without indicators retain the local UI and
			// sync supervisor. Availability is diagnostic; never claim an icon is visible.
			actions <- tray.Action{Kind: "ready"}
			<-ctx.Done()
			err = nil
		}
	}
	if shutdownErr := shutdown(); shutdownErr != nil {
		return shutdownErr
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func trayView(s State) tray.View {
	kind := trayState(s.Kind)
	v := tray.View{State: kind, Text: s.Text, Autostart: s.Autostart, Busy: s.Busy, AccountError: s.AccountError, ConflictCount: s.ConflictCount, ConflictsKnown: s.ConflictsKnown}
	for _, account := range s.Accounts {
		v.Accounts = append(v.Accounts, tray.Account{ID: string(account.ID), Name: account.Name, State: account.State, Configured: account.Configured, Enabled: account.Enabled, CanRescan: account.CanRescan, LastScan: account.LastScan})
	}
	for _, m := range s.Mounts {
		state, _ := mountState(m)
		v.Mounts = append(v.Mounts, tray.Mount{ID: string(m.ID), Name: m.Name, Mountpoint: m.Mountpoint, State: trayState(state), Running: m.Status.Running, Desired: m.Desired, Pending: m.Status.Queued + m.Status.Inflight + m.Status.VFSPending, Conflicts: m.Status.Conflicts, StatusUnavailable: m.StatusUnavailable, Error: mountError(m)})
	}
	return v
}

// Keep the private bootstrap fragment while selecting the existing conflict UI.
func managementViewURL(address, view string) (string, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", err
	}
	if view != "" {
		query := u.Query()
		query.Set("view", view)
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func trayState(kind string) string {
	switch kind {
	case "synced":
		return "ready"
	case "unauthorized":
		return "auth"
	case "setup", "starting", "offline":
		return "unavailable"
	}
	return kind
}

func mountError(m MountView) string {
	if m.Error != "" {
		return m.Error
	}
	return m.Status.LastError
}

// The desktop owns this goroutine and waits for it before releasing its singleton.
func startDiscovery(ctx context.Context, runner interface{ RunDiscovery(context.Context) }, operations *sync.WaitGroup) {
	operations.Add(1)
	go func() { defer operations.Done(); runner.RunDiscovery(ctx) }()
}

// Stop accepting work, drain its only producer, then close owned resources before
// native Quit can terminate the process. Concurrent quit paths share one result.
func desktopShutdown(cancel context.CancelFunc, finished <-chan error, closeUI, closeManager func() error, release func()) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			cancel()
			result = <-finished
			if closeUI != nil {
				result = errors.Join(result, closeUI())
			}
			if closeManager != nil {
				result = errors.Join(result, closeManager())
			}
			if release != nil {
				release()
			}
		})
		return result
	}
}
