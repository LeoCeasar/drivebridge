package feishu

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/spf13/cobra"
)

var errLinkClipboard = errors.New("unable to copy link to clipboard")

type linkActions struct {
	resolve     func(context.Context, *manager.Manager, string) (manager.LinkResult, error)
	copy        func(string) error
	prepareCopy func(context.Context) (func(string) error, error)
	feedback    func(context.Context, string, bool)
}

func newLinkCommand(o CommandOptions, actions linkActions) *cobra.Command {
	if actions.resolve == nil {
		actions.resolve = func(ctx context.Context, m *manager.Manager, path string) (manager.LinkResult, error) {
			return m.Link(ctx, path)
		}
	}
	if actions.copy == nil && actions.prepareCopy == nil {
		actions.prepareCopy = platform.NewLinkClipboardWriter
	}
	if actions.feedback == nil {
		actions.feedback = platform.LinkFeedback
	}
	var paths []string
	var copyLink bool
	var timeout time.Duration
	c := &cobra.Command{Use: "link --path PATH", Short: "Get or copy the existing Feishu link for one mounted file or folder", SilenceUsage: true, SilenceErrors: true}
	c.Flags().StringArrayVar(&paths, "path", nil, "One absolute path inside a DriveBridge mount (specify once)")
	c.Flags().BoolVar(&copyLink, "copy", false, "Copy the link and show desktop feedback")
	c.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "Maximum time for link resolution, including local setup")
	c.Args = func(_ *cobra.Command, args []string) error {
		if len(args) != 0 || len(paths) != 1 || paths[0] == "" || timeout <= 0 {
			return manager.ErrConfig
		}
		return nil
	}
	// Desktop feedback must be shown before the process exits, including when
	// manager construction or credential verification fails.
	noExit := o
	noExit.Exit = nil
	c.RunE = func(c *cobra.Command, args []string) error {
		previousContext := c.Context()
		signalContext, stop := signal.NotifyContext(previousContext, os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(signalContext, timeout)
		defer cancel()
		c.SetContext(ctx)
		defer c.SetContext(previousContext)
		invocation := actions
		runOptions := noExit
		factory := runOptions.NewManager
		if factory == nil {
			factory = ManagerForCommand
		}
		runOptions.NewManager = func(c *cobra.Command) (*manager.Manager, error) {
			if copyLink && invocation.prepareCopy != nil {
				var err error
				invocation.copy, err = invocation.prepareCopy(c.Context())
				if err != nil {
					return nil, reportLinkClipboardFailure(err, invocation.feedback)
				}
			}
			return factory(c)
		}
		run := commandRun(runOptions, func(ctx context.Context, m *manager.Manager, _ []string) (any, error) {
			result, err := invocation.resolve(ctx, m, paths[0])
			if err != nil {
				return nil, err
			}
			result, err = finishLink(ctx, result, copyLink, invocation)
			if copyLink && flagString(c, "json") != "true" {
				return nil, err
			}
			return result, err
		})
		err := run(c, args)
		if err != nil {
			if copyLink && !errors.Is(err, errLinkClipboard) {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				actions.feedback(ctx, linkFailureMessage(err), true)
				cancel()
			}
			if o.Exit != nil {
				o.Exit(manager.ExitCode(err))
			}
		}
		return err
	}
	return c
}

func finishLink(ctx context.Context, result manager.LinkResult, copyLink bool, actions linkActions) (manager.LinkResult, error) {
	if larkcli.SafeResourceURL(result.URL) == "" {
		return manager.LinkResult{}, manager.ErrLinkUnavailable
	}
	if !copyLink {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return manager.LinkResult{}, err
	}
	if err := actions.copy(result.URL); err != nil {
		return manager.LinkResult{}, reportLinkClipboardFailure(err, actions.feedback)
	}
	message := "飞书分享链接已复制，访问权限保持不变。"
	if result.PendingChanges {
		message += "此项目有待同步修改，链接指向云端当前内容。"
	}
	if result.Cached {
		message += "使用已缓存链接，当前访问状态未核验。"
	}
	actions.feedback(ctx, message, false)
	return result, nil
}

func reportLinkClipboardFailure(err error, feedback func(context.Context, string, bool)) error {
	message := "无法写入剪贴板，请稍后重试。"
	if errors.Is(err, platform.ErrLinkClipboardChanged) {
		message = "剪贴板已变化，本次结果未覆盖新内容，请重新复制链接。"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	feedback(ctx, message, true)
	return errLinkClipboard
}

func linkFailureMessage(err error) string {
	info := manager.ErrorInfo(err)
	switch info.Code {
	case "link_unsupported", "link_not_uploaded", "link_unavailable", "link_lookup_timeout":
		return info.Message
	case "unauthorized", "credential_unavailable":
		return "无法获取飞书链接，请在 DriveBridge 中检查账号授权。"
	case "identity_changed", "version_changed":
		return "文件或账号状态已变化，请重新选择后重试。"
	case "pending", "starting", "worker_unavailable":
		return "文件或挂载正在处理，请稍后重试。"
	case "network":
		return "查询飞书链接超时或连接失败，请稍后重试。"
	default:
		return "无法获取飞书链接，请在 DriveBridge 中检查挂载和同步状态。"
	}
}
