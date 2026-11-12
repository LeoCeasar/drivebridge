package feishu

import (
	"context"
	"io"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/spf13/cobra"
)

func historyProfile(ctx context.Context, m *manager.Manager, c *cobra.Command, required bool) (model.ProfileID, error) {
	name := flagString(c, "profile")
	if name == "" {
		if required {
			return "", manager.ErrConfig
		}
		return "", nil
	}
	p, err := m.ResolveProfile(ctx, name)
	return p.ID, err
}
func installHistoryCommands(root *cobra.Command, o CommandOptions) {
	group := &cobra.Command{Use: "history", Short: "Browse private Git history, compare versions and safely restore files"}
	group.PersistentFlags().String("profile", "", "Profile name or UUID; omit for the global list")
	list := &cobra.Command{Use: "list", Args: exactArgs(0)}
	list.Flags().String("cursor", "", "Continuation cursor")
	list.Flags().String("entry", "", "Stable entry UUID")
	list.Flags().String("search", "", "File name or path substring")
	list.Flags().Int("limit", 50, "Number of history records (1-200)")
	list.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		p, err := historyProfile(ctx, m, list, false)
		if err != nil {
			return nil, err
		}
		limit, _ := list.Flags().GetInt("limit")
		if limit < 1 || limit > 200 {
			return nil, manager.ErrConfig
		}
		return m.HistoryList(ctx, manager.HistoryQuery{ProfileID: p, Cursor: flagString(list, "cursor"), Query: history.Query{EntryID: model.EntryID(flagString(list, "entry")), Search: flagString(list, "search"), Limit: limit}})
	})
	show := &cobra.Command{Use: "show VERSION", Args: exactArgs(1)}
	show.Flags().Bool("content", false, "Stream the captured body to stdout instead of metadata")
	show.Flags().String("role", "", "local, remote, draft or an explicit captured role")
	show.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		p, err := historyProfile(ctx, m, show, true)
		if err != nil {
			return nil, err
		}
		if flagString(show, "content") == "true" {
			body, err := m.HistoryOpenBody(ctx, p, args[0], flagString(show, "role"))
			if err != nil {
				return nil, err
			}
			defer body.Close()
			_, err = io.Copy(show.OutOrStdout(), body)
			return nil, err
		}
		return m.HistoryDetail(ctx, p, args[0])
	})
	diff := &cobra.Command{Use: "diff LEFT RIGHT", Args: exactArgs(2)}
	diff.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		p, err := historyProfile(ctx, m, diff, true)
		if err != nil {
			return nil, err
		}
		d, err := m.HistoryDiff(ctx, p, args[0], args[1])
		if err != nil {
			return nil, err
		}
		if flagString(diff, "json") == "true" || d.Binary || d.Limited {
			return d, nil
		}
		_, err = io.WriteString(diff.OutOrStdout(), d.Patch)
		return nil, err
	})
	restore := &cobra.Command{Use: "restore VERSION", Short: "Create a new local edit from a recorded version; normal cloud checks still apply", Args: exactArgs(1)}
	restore.Flags().String("request-id", "", "Idempotency UUID; reuse after an interrupted request")
	restore.Flags().Uint64("expected-generation", 0, "Require this current file generation")
	restore.Flags().String("role", "", "Captured body role")
	restore.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		p, err := historyProfile(ctx, m, restore, true)
		if err != nil {
			return nil, err
		}
		detail, err := m.HistoryDetail(ctx, p, args[0])
		if err != nil {
			return nil, err
		}
		if !detail.RestoreAvailable || detail.Current == nil {
			return nil, manager.ErrPending
		}
		current := detail.Current
		generation := current.Entry.Generation
		if restore.Flags().Changed("expected-generation") {
			generation, _ = restore.Flags().GetUint64("expected-generation")
		}
		id := flagString(restore, "request-id")
		if id == "" {
			id = uuid.NewString()
		}
		return m.HistoryRestore(ctx, model.HistoryRestoreRequest{ProfileID: p, MountID: detail.Version.Event.MountID, EntryID: current.Entry.ID, VersionID: args[0], Role: flagString(restore, "role"), RequestID: id, ExpectedGeneration: generation, ExpectedPath: current.Entry.Remote, ExpectedDeleted: current.Deleted, ParentID: current.Parent.ID, ParentGeneration: current.Parent.Generation})
	})
	status := &cobra.Command{Use: "status", Args: exactArgs(0), RunE: commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) { return m.HistoryStates(ctx) })}
	sync := &cobra.Command{Use: "sync MOUNT", Short: "Archive existing local history inputs without downloading cloud files", Args: exactArgs(1)}
	sync.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
		mount, err := m.ResolveMount(ctx, args[0])
		if err != nil {
			return nil, err
		}
		return m.HistorySync(ctx, mount.ID)
	})

	compact := &cobra.Command{Use: "compact", Args: exactArgs(0)}
	compact.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		p, e := historyProfile(ctx, m, compact, true)
		if e != nil {
			return nil, e
		}
		return m.HistoryCompact(ctx, p)
	})
	policy := &cobra.Command{Use: "policy", Short: "Show or set retention settings for an account", Args: exactArgs(0)}
	policy.Flags().Int("keep-days", 30, "Minimum age retention in days")
	policy.Flags().Int("minimum-versions", 10, "Minimum distinct versions per file")
	policy.Flags().Int64("max-bytes", 5<<30, "Soft history budget in bytes")
	policy.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
		p, e := historyProfile(ctx, m, policy, true)
		if e != nil {
			return nil, e
		}
		value, e := m.HistoryPolicy(ctx, p)
		if e != nil {
			return nil, e
		}
		changed := false
		if policy.Flags().Changed("keep-days") {
			value.KeepDays, _ = policy.Flags().GetInt("keep-days")
			changed = true
		}
		if policy.Flags().Changed("minimum-versions") {
			value.MinimumVersions, _ = policy.Flags().GetInt("minimum-versions")
			changed = true
		}
		if policy.Flags().Changed("max-bytes") {
			value.MaxBytes, _ = policy.Flags().GetInt64("max-bytes")
			changed = true
		}
		if changed {
			if e = m.SetHistoryPolicy(ctx, p, value); e != nil {
				return nil, e
			}
		}
		return value, nil
	})
	for _, pin := range []bool{true, false} {
		verb := "pin"
		if !pin {
			verb = "unpin"
		}
		command := &cobra.Command{Use: verb + " VERSION", Args: exactArgs(1)}
		command.RunE = commandRun(o, func(ctx context.Context, m *manager.Manager, args []string) (any, error) {
			p, e := historyProfile(ctx, m, command, true)
			if e != nil {
				return nil, e
			}
			return map[string]bool{"pinned": pin}, m.HistoryPin(ctx, p, args[0], pin)
		})
		group.AddCommand(command)
	}
	group.AddCommand(compact, policy)
	group.AddCommand(list, show, diff, restore, status, sync)
	root.AddCommand(group)
}
