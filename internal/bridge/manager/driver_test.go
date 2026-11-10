package manager

import (
	"strings"
	"testing"
)

func TestArtifactPinsRejectCrossPlatformAndIncompleteSigningProof(t *testing.T) {
	mac := "a8f652c2a62577a152467a983ec60548570fb0a08715e95fa3f579438c81885b"
	linux := "a2f55a5b7cb618f2635e1034f4e8196146c9c055c3995035322ba14382be66ba"
	signed := strings.Repeat("1", 64)
	for _, tc := range []struct {
		platform, actual, post, upstream string
		want                             bool
	}{{"darwin/arm64", mac, "", "", true}, {"linux/amd64", linux, "", "", true}, {"linux/amd64", mac, "", "", false}, {"darwin/arm64", signed, signed, mac, true}, {"darwin/arm64", signed, signed, "", false}, {"darwin/arm64", signed, signed, linux, false}, {"darwin/arm64", mac, signed, mac, false}, {"darwin/arm64", mac, "", mac, false}, {"darwin/arm64", signed, "BAD", mac, false}, {"unsupported/arm64", signed, signed, mac, false}} {
		if got := artifactAccepted(tc.platform, tc.actual, tc.post, tc.upstream); got != tc.want {
			t.Fatalf("artifact policy mismatch for %s signed=%v", tc.platform, tc.post != "")
		}
	}
}
