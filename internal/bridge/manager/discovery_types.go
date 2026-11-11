package manager

import (
	"context"
	"net/url"
	"time"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
)

type DiscoveryView struct {
	ProfileID  model.ProfileID    `json:"profile_id"`
	Configured bool               `json:"configured"`
	Enabled    bool               `json:"enabled"`
	Origin     string             `json:"origin,omitempty"`
	State      string             `json:"state"`
	Error      string             `json:"error,omitempty"`
	LastScan   time.Time          `json:"last_scan,omitempty"`
	Snapshot   discovery.Snapshot `json:"snapshot"`
	MountID    model.MountID      `json:"mount_id,omitempty"`
	Mountpoint string             `json:"mountpoint,omitempty"`
	Browser    *BrowserBridgeView `json:"browser,omitempty"`
}

type BrowserBridgeView struct {
	ExtensionID     string `json:"extension_id,omitempty"`
	ExtensionPath   string `json:"extension_path,omitempty"`
	Connected       bool   `json:"connected"`
	HelperAvailable bool   `json:"helper_available"`
}

type DiscoveryCandidate struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	URL      string `json:"url,omitempty"`
	nodeID   string
	parentID string
}

type discoverySession interface {
	Get(context.Context, string, url.Values) ([]byte, error)
	Login(context.Context) error
	SealLogin(context.Context) error
	Close() error
}

type discoverySessionFactory func(context.Context, model.ProfileID, string) (discoverySession, error)

type discoverySettings struct {
	Binding   discovery.Binding `json:"binding"`
	RootToken string            `json:"root_token"`
	Enabled   bool              `json:"enabled"`
	State     string            `json:"state"`
	Error     string            `json:"error,omitempty"`
	Attempted time.Time         `json:"attempted,omitempty"`
}
