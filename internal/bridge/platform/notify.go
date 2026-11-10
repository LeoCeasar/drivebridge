package platform

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

var ErrNoDesktop = errors.New("interactive desktop unavailable")

func validBrowserURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && !strings.ContainsAny(raw, "\x00\r\n")
}
func OpenBrowser(ctx context.Context, raw string) error {
	if !validBrowserURL(raw) {
		return errors.New("unsupported browser URL")
	}
	if !DesktopAvailable() {
		return ErrNoDesktop
	}
	return openBrowser(ctx, raw)
}
func Notify(ctx context.Context, title, message string) error {
	if !DesktopAvailable() {
		return ErrNoDesktop
	}
	return notify(ctx, title, message)
}
