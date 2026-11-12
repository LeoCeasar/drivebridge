package ui

import (
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
)

const accountRootPrefix = "账号盘-"

// describeMount adds presentation-only fields. Name stays the raw stored value
// (the page keeps it as a tooltip); DisplayName is what people should read.
func describeMount(view *MountView, remotes []model.Remote, profiles []model.Profile) {
	var remote model.Remote
	for _, r := range remotes {
		if r.ID == view.RemoteID {
			remote = r
		}
	}
	view.ProfileID = remote.ProfileID
	for _, p := range profiles {
		if p.ID == remote.ProfileID {
			view.AccountName = p.Name
		}
	}
	// Automatic mounts are named "账号盘-<profile id>"; legacy rows may lack
	// the AccountRoot flag but still carry that generated name.
	view.AccountRoot = remote.AccountRoot || (remote.ID != "" && strings.HasPrefix(view.Name, accountRootPrefix) && strings.HasSuffix(view.Name, string(remote.ProfileID)))
	if view.AccountRoot {
		if view.AccountName != "" {
			view.DisplayName = "DriveBridge — " + view.AccountName
		} else {
			view.DisplayName = "DriveBridge 账号盘"
		}
	}
}
