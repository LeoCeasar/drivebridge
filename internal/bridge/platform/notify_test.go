package platform

import (
	"context"
	"testing"
)

func TestBrowserURLRejectsExecutableAndCredentialSchemes(t *testing.T) {
	for _, u := range []string{"file:///private/data", "javascript:alert(1)", "https://user:secret@example.com", "https://example.com\nanything", "http:///missing"} {
		if err := OpenBrowser(context.Background(), u); err == nil {
			t.Fatalf("unsafe URL accepted: %s", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:1234/#bootstrap=value", "https://example.com/login"} {
		if !validBrowserURL(u) {
			t.Fatalf("safe URL rejected: %s", u)
		}
	}
}
