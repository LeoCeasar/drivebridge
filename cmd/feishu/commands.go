package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/spf13/cobra"
)

func exactArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return manager.ErrConfig
		}
		return nil
	}
}
func installCommands(root *cobra.Command, o CommandOptions) {
	doctor := &cobra.Command{Use: "doctor", Short: "Check compiled mount engine, driver, CLI and optional account", Args: exactArgs(0)}
	doctor.Flags().String("profile", "", "Verify this configured profile")
	doctor.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		var id model.ProfileID
		if name := flagString(doctor, "profile"); name != "" {
			p, err := m.ResolveProfile(ctx, name)
			if err != nil {
				return nil, err
			}
			id = p.ID
		}
		return m.Doctor(ctx, id)
	})
	root.AddCommand(doctor)
	account := &cobra.Command{Use: "account", Short: "Manage this computer's personal account bindings"}
	account.PersistentFlags().String("profile", "", "Profile name or UUID")
	account.PersistentFlags().String("brand", "feishu", "feishu or lark")
	for _, action := range []string{"login", "recover", "status", "logout", "unlink"} {
		action := action
		c := &cobra.Command{Use: action, Args: exactArgs(0)}
		c.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
			name := flagString(c, "profile")
			if name == "" {
				return nil, manager.ErrConfig
			}
			p, err := m.ResolveProfile(ctx, name)
			if err != nil && action == "login" {
				p, err = m.AddProfile(ctx, name, flagString(c, "brand"))
			}
			if err != nil {
				return nil, err
			}
			a, err := m.Auth(ctx)
			if err != nil {
				return nil, err
			}
			switch action {
			case "status":
				return a.Status(ctx, p.ID)
			case "logout":
				return map[string]bool{"logged_out": true}, a.Logout(ctx, p.ID)
			case "unlink":
				return map[string]bool{"unlinked": true}, a.Unlink(ctx, p.ID)
			default:
				return authorize(ctx, c, a, p.ID, action == "recover")
			}
		})
		account.AddCommand(c)
	}
	root.AddCommand(account)
	remote := &cobra.Command{Use: "remote", Short: "Configure shared-folder mappings"}
	addRemote := &cobra.Command{Use: "add NAME", Args: exactArgs(1)}
	addRemote.Flags().String("profile", "", "Configured profile")
	addRemote.Flags().String("folder-token", "", "Shared folder token")
	addRemote.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		p, err := m.ResolveProfile(ctx, flagString(addRemote, "profile"))
		if err != nil {
			return nil, err
		}
		return m.AddRemote(ctx, args[0], p.ID, flagString(addRemote, "folder-token"))
	})
	remote.AddCommand(addRemote)
	remote.AddCommand(&cobra.Command{Use: "list", Args: exactArgs(0), RunE: commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		c, err := m.LoadConfig(ctx)
		return c.Remotes, err
	})})
	for _, action := range []string{"test", "remove"} {
		action := action
		c := &cobra.Command{Use: action + " NAME", Args: exactArgs(1)}
		c.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
			r, err := m.ResolveRemote(ctx, args[0])
			if err != nil {
				return nil, err
			}
			if action == "test" {
				return m.TestRemote(ctx, r.ID)
			}
			return map[string]bool{"removed": true}, m.RemoveRemote(ctx, r.ID)
		})
		remote.AddCommand(c)
	}
	root.AddCommand(remote)
	mount := &cobra.Command{Use: "mount", Short: "Create, start, inspect and safely stop managed mounts"}
	defaults := config.DefaultMount()
	addMount := &cobra.Command{Use: "add NAME", Args: exactArgs(1)}
	addMount.Flags().String("remote", "", "Configured shared folder")
	addMount.Flags().String("mountpoint", "", "Local mount path or Windows drive letter")
	addMount.Flags().String("cache-mode", "full", "Managed mounts require full cache")
	addMount.Flags().String("cache-size", "5G", "Soft clean-cache budget")
	addMount.Flags().String("cache-max-age", defaults.CacheMaxAge, "Unused synced body retention; 0h uses only the capacity budget")
	addMount.Flags().String("macos-backend", "", "macOS mount backend: kernel or fskit (empty keeps the default)")
	addMount.Flags().StringSlice("background-pull-exclude", defaults.BackgroundPullExcludedExtensions, "Suffixes excluded from background body refresh; explicit reads and local uploads remain enabled")
	addMount.Flags().String("write-back", defaults.WriteBack, "Local write-back delay")
	addMount.Flags().Bool("notify", true, "Notify about conflicts when a desktop is available")
	addMount.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		r, err := m.ResolveRemote(ctx, flagString(addMount, "remote"))
		if err != nil {
			return nil, err
		}
		v := config.DefaultMount()
		v.Name = args[0]
		v.RemoteID = r.ID
		v.Mountpoint = flagString(addMount, "mountpoint")
		v.CacheMode = flagString(addMount, "cache-mode")
		v.MacOSBackend = flagString(addMount, "macos-backend")
		var size fs.SizeSuffix
		if err = size.Set(flagString(addMount, "cache-size")); err != nil {
			return nil, manager.ErrConfig
		}
		v.CacheMaxBytes = int64(size)
		v.CacheMaxAge = flagString(addMount, "cache-max-age")
		v.BackgroundPullExcludedExtensions, _ = addMount.Flags().GetStringSlice("background-pull-exclude")
		if addMount.Flags().Changed("background-pull-exclude") && v.BackgroundPullExcludedExtensions == nil {
			v.BackgroundPullExcludedExtensions = []string{}
		}
		v.WriteBack = flagString(addMount, "write-back")
		v.Notify = flagString(addMount, "notify") == "true"
		return m.AddMount(ctx, v)
	})
	mount.AddCommand(addMount)
	for _, kind := range []string{"remove", "restore"} {
		kind := kind
		entry := &cobra.Command{Use: kind + " NAME", Args: exactArgs(1), Short: "Delete a clean local mapping and caches, or restore a legacy archived mapping; cloud files remain"}
		entry.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
			v, err := m.ResolveMount(ctx, args[0])
			if err != nil {
				if kind == "remove" && errors.Is(err, manager.ErrConfig) {
					return map[string]bool{"deleted": true}, m.DeleteMount(ctx, model.MountID(args[0]))
				}
				return nil, err
			}
			if kind == "remove" {
				err = m.DeleteMount(ctx, v.ID)
			} else {
				err = m.RestoreMount(ctx, v.ID)
			}
			return map[string]any{"deleted": kind == "remove", "cloud_files_preserved": true, "history_preserved": true}, err
		})
		mount.AddCommand(entry)
	}

	policy := &cobra.Command{Use: "pull-policy NAME", Args: exactArgs(1), Short: "Inspect or change suffixes excluded from background downloads"}
	policy.Flags().StringSlice("exclude", nil, "Excluded suffixes; use --exclude= to allow all")
	policy.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		target, err := m.ResolveMount(ctx, args[0])
		if err != nil {
			return nil, err
		}
		if policy.Flags().Changed("exclude") {
			rules, _ := policy.Flags().GetStringSlice("exclude")
			if rules == nil {
				rules = []string{}
			}
			if err = m.SetBackgroundPullPolicy(ctx, target.ID, rules); err != nil {
				return nil, err
			}
			target, err = m.ResolveMount(ctx, args[0])
			if err != nil {
				return nil, err
			}
		}
		rules, err := config.EffectiveBackgroundPullExcludedExtensions(target)
		return map[string]any{"background_pull_excluded_extensions": rules}, err
	})
	mount.AddCommand(policy)

	start := &cobra.Command{Use: "start NAME", Args: exactArgs(1)}
	start.Flags().Bool("background", false, "Return after the worker and mount are ready")
	start.Flags().Duration("wait", 30*time.Second, "Maximum time to wait for readiness")
	start.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		v, err := m.ResolveMount(ctx, args[0])
		if err != nil {
			return nil, err
		}
		background := flagString(start, "background") == "true"
		status, err := m.Start(ctx, v.ID, background)
		if err != nil || background {
			return status, err
		}
		waitCtx, cancel := signal.NotifyContext(ctx, os.Interrupt)
		defer cancel()
		err = m.Wait(waitCtx, v.ID)
		if errors.Is(err, context.Canceled) {
			err = m.Stop(context.Background(), v.ID, 60*time.Second)
		}
		return status, err
	})
	mount.AddCommand(start)
	status := &cobra.Command{Use: "status [NAME]", Args: cobra.MaximumNArgs(1)}
	status.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		if len(args) == 1 {
			v, err := m.ResolveMount(ctx, args[0])
			if err != nil {
				return nil, err
			}
			return m.Status(ctx, v.ID)
		}
		c, err := m.LoadConfig(ctx)
		if err != nil {
			return nil, err
		}
		out := []model.MountStatus{}
		for _, v := range c.Mounts {
			s, err := m.Status(ctx, v.ID)
			if err != nil {
				return out, err
			}
			out = append(out, s)
		}
		return out, nil
	})
	mount.AddCommand(status)
	stop := &cobra.Command{Use: "stop NAME", Args: exactArgs(1)}
	stop.Flags().Duration("wait-upload", 60*time.Second, "Wait for open handles, VFS data and cloud work")
	stop.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		v, err := m.ResolveMount(ctx, args[0])
		if err != nil {
			return nil, err
		}
		wait, err := time.ParseDuration(flagString(stop, "wait-upload"))
		if err != nil {
			return nil, manager.ErrConfig
		}
		return map[string]bool{"stopped": true}, m.Stop(ctx, v.ID, wait)
	})
	mount.AddCommand(stop)
	stopAll := &cobra.Command{Use: "stop-all", Args: exactArgs(0)}
	stopAll.Flags().Duration("wait-upload", 60*time.Second, "Wait for each worker to drain safely")
	stopAll.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		wait, err := time.ParseDuration(flagString(stopAll, "wait-upload"))
		if err != nil {
			return nil, manager.ErrConfig
		}
		return map[string]bool{"stopped": true}, m.StopAll(ctx, wait)
	})
	mount.AddCommand(stopAll)
	for _, action := range []string{"enable-autostart", "disable-autostart"} {
		action := action
		c := &cobra.Command{Use: action + " NAME", Args: exactArgs(1)}
		c.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
			v, err := m.ResolveMount(ctx, args[0])
			if err != nil {
				return nil, err
			}
			return map[string]bool{"autostart": action == "enable-autostart"}, m.SetAutostart(ctx, v.ID, action == "enable-autostart")
		})
		mount.AddCommand(c)
	}
	root.AddCommand(mount)
	upload := &cobra.Command{Use: "upload", Short: "Inspect or reconcile queued cloud writes"}
	for _, action := range []string{"status", "retry"} {
		action := action
		c := &cobra.Command{Use: action + " NAME", Args: exactArgs(1)}
		c.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
			v, err := m.ResolveMount(ctx, args[0])
			if err != nil {
				return nil, err
			}
			if action == "status" {
				return m.UploadStatus(ctx, v.ID)
			}
			return map[string]bool{"reconciled": true}, m.Retry(ctx, v.ID)
		})
		upload.AddCommand(c)
	}
	root.AddCommand(upload)
	conflict := &cobra.Command{Use: "conflict", Short: "Inspect conflicts, open the merge UI, or submit an explicit merge"}
	conflict.AddCommand(&cobra.Command{Use: "list NAME", Args: exactArgs(1), RunE: commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		v, err := m.ResolveMount(ctx, args[0])
		if err != nil {
			return nil, err
		}
		views, err := m.Conflicts(ctx, v.ID)
		out := []model.Conflict{}
		for _, view := range views {
			out = append(out, view.Conflict)
		}
		return out, err
	})})
	openConflict := &cobra.Command{Use: "open ID", Args: exactArgs(1)}
	openConflict.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		if _, err := m.Conflict(ctx, model.ConflictID(args[0])); err != nil {
			return nil, err
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		command := exec.CommandContext(ctx, exe, "feishu", "--bridge-config", m.ConfigFile(), "gui", "--conflict", args[0])
		command.Stdout = openConflict.OutOrStdout()
		command.Stderr = openConflict.ErrOrStderr()
		return nil, command.Run()
	})
	conflict.AddCommand(openConflict)
	resolve := &cobra.Command{Use: "resolve ID", Args: exactArgs(1)}
	resolve.Flags().String("file", "", "Merged file to submit")
	resolve.Flags().String("action", "", "Deletion decision: restore_local or accept_delete")
	resolve.Flags().Uint64("expected-generation", 0, "Captured local generation")
	resolve.Flags().String("expected-remote-revision", "", "Captured conflict revision or deletion marker")
	resolve.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		action, filename := flagString(resolve, "action"), flagString(resolve, "file")
		if (action != "" && action != model.ResolveRestoreLocal && action != model.ResolveAcceptDelete) || (action == "" && filename == "") || (action == model.ResolveAcceptDelete && filename != "") {
			return nil, manager.ErrConfig
		}
		var body []byte
		if filename != "" {
			file, err := os.Open(filename)
			if err != nil {
				return nil, manager.ErrConfig
			}
			defer file.Close()
			body, err = io.ReadAll(io.LimitReader(file, (80<<20)+1))
			if err != nil {
				return nil, err
			}
			if len(body) > 80<<20 {
				return nil, model.ErrUnsupportedContent
			}
		}
		generation, err := resolve.Flags().GetUint64("expected-generation")
		if err != nil {
			return nil, manager.ErrConfig
		}
		return map[string]bool{"accepted": true}, m.ResolveConflict(ctx, model.ResolveRequest{Action: action, ConflictID: model.ConflictID(args[0]), ExpectedGeneration: generation, ExpectedRemoteRevision: flagString(resolve, "expected-remote-revision"), Content: body})
	})
	conflict.AddCommand(resolve)
	conflict.AddCommand(&cobra.Command{Use: "export ID", Short: "Export immutable local conflict copies without overwriting edited copies", Args: exactArgs(1), RunE: commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		return m.ExportConflict(ctx, model.ConflictID(args[0]))
	})})
	root.AddCommand(conflict)
	cache := &cobra.Command{Use: "cache", Short: "Inspect caches or remove only unreferenced clean data"}
	for _, action := range []string{"status", "clean"} {
		action := action
		c := &cobra.Command{Use: action + " NAME", Args: exactArgs(1)}
		c.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
			v, err := m.ResolveMount(ctx, args[0])
			if err != nil {
				return nil, err
			}
			if action == "status" {
				return m.CacheStatus(ctx, v.ID)
			}
			return m.CleanCache(ctx, v.ID)
		})
		cache.AddCommand(c)
	}
	root.AddCommand(cache)
	logs := &cobra.Command{Use: "logs NAME", Short: "Read structured, redacted worker events", Args: exactArgs(1)}
	logs.Flags().Bool("follow", false, "Follow newly recorded events")
	logs.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		v, err := m.ResolveMount(ctx, args[0])
		if err != nil {
			return nil, err
		}
		if flagString(logs, "follow") != "true" {
			return m.Logs(ctx, v.ID)
		}
		followCtx, cancel := signal.NotifyContext(ctx, os.Interrupt)
		defer cancel()
		seen := map[string]bool{}
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			events, err := m.Logs(followCtx, v.ID)
			if err != nil {
				if followCtx.Err() != nil {
					return nil, nil
				}
				return nil, err
			}
			for _, event := range events {
				key := event.At.String() + "/" + event.Code
				if !seen[key] {
					seen[key] = true
					if err := json.NewEncoder(logs.OutOrStdout()).Encode(event); err != nil {
						return nil, err
					}
				}
			}
			select {
			case <-followCtx.Done():
				return nil, nil
			case <-tick.C:
			}
		}
	})
	root.AddCommand(logs)
	completion := &cobra.Command{Use: "completion SHELL", Args: exactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		executable := c.Root()
		switch strings.ToLower(args[0]) {
		case "bash":
			return executable.GenBashCompletionV2(c.OutOrStdout(), true)
		case "zsh":
			return executable.GenZshCompletion(c.OutOrStdout())
		case "fish":
			return executable.GenFishCompletion(c.OutOrStdout(), true)
		case "powershell":
			return executable.GenPowerShellCompletionWithDesc(c.OutOrStdout())
		default:
			return manager.ErrConfig
		}
	}}
	root.AddCommand(completion)
	worker := &cobra.Command{Use: "_worker", Hidden: true, Args: exactArgs(0)}
	worker.Flags().String("mount-id", "", "Mount UUID")
	worker.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		return nil, m.RunWorker(ctx, model.MountID(flagString(worker, "mount-id")))
	})
	root.AddCommand(worker)
}
func authorize(ctx context.Context, c *cobra.Command, a *auth.Service, id model.ProfileID, recovery bool) (any, error) {
	for {
		var flow auth.AuthFlow
		var err error
		if recovery {
			_, _ = fmt.Fprintln(c.ErrOrStderr(), "Stopping related mounts and preserving pending data before same-account authorization.")
			flow, err = a.StartRecovery(ctx, id)
		} else {
			flow, err = a.Start(ctx, id)
		}
		if errors.Is(err, auth.ErrNeedsReconcile) {
			identity, reconcileErr := a.Reconcile(ctx, id)
			if reconcileErr == nil {
				return identity, nil
			}
			if errors.Is(reconcileErr, auth.ErrUserMissing) {
				continue
			}
			return nil, reconcileErr
		}
		if err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintln(c.ErrOrStderr(), flow.URL)
		identity, err := a.Complete(ctx, flow.ID)
		if errors.Is(err, auth.ErrUserMissing) {
			continue
		}
		return identity, err
	}
}
