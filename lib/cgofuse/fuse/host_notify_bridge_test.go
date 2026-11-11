//go:build darwin && cgo && cgofuse_bridge_test

package fuse

import "testing"

func TestHostNotifyOptionalMacInvalidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome int
		handled bool
	}{{"optional symbol missing", 0, false}, {"cache invalidated", 1, true}, {"path not cached", 2, true}, {"native error", 3, false}} {
		t.Run(test.name, func(t *testing.T) {
			if got := notifyBridgeProbe(test.outcome); got != test.handled {
				t.Fatalf("handled=%t want%t", got, test.handled)
			}
		})
	}
}
