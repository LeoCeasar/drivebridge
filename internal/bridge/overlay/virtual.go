package overlay

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Only the provider's actual HTTPS URL is projected. Nothing is constructed from
// a token/tenant, and shortcut line injection or embedded credentials are rejected.
func safeSourceURL(raw string) string {
	if strings.ContainsRune(raw, '\\') || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.ContainsRune(decoded, '\\') || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return ""
	}
	return raw
}
func virtualContent(m store.Metadata) []byte {
	if link := safeSourceURL(m.SourceURL); link != "" {
		return []byte("[InternetShortcut]\r\nURL=" + link + "\r\n")
	}
	return []byte(fmt.Sprintf("This online resource (type %q) is read-only and is not converted by DriveBridge.\nThe provider did not supply a safe HTTPS link. Open it in the provider's application.\n", m.VirtualType))
}
func (f *Fs) requireWritable(ctx context.Context, e model.Entry) error {
	if isDocAssetPath(e.Remote) {
		return fs.ErrorPermissionDenied
	}
	if source, ok := f.source.(accountProtection); ok && source.accountProtected(e) {
		return fs.ErrorPermissionDenied
	}
	m, err := f.store.Metadata(ctx, e.ID)
	if err != nil {
		return err
	}
	if m.VirtualType != "" {
		return fs.ErrorPermissionDenied
	}
	return nil
}

func (f *Fs) requireParent(ctx context.Context, remote string) error {
	if isDocAssetPath(remote) {
		return fs.ErrorPermissionDenied
	}
	e, err := f.directory(ctx, remote)
	if err != nil {
		return err
	}
	if source, ok := f.source.(accountProtection); ok && source.accountSynthetic(e.Token) {
		return fs.ErrorPermissionDenied
	}
	return nil
}
