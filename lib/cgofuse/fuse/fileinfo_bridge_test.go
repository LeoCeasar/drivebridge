//go:build cgo && cgofuse_bridge_test

package fuse

import (
	"runtime"
	"testing"
)

func TestFileInfoBridgeTransfersMacPurgeFlagsAndPreservesIOFlags(t *testing.T) {
	for _, test := range []struct {
		info FileInfo_t
		mask uint32
	}{
		{FileInfo_t{Fh: 42}, 0},
		{FileInfo_t{Fh: 43, PurgeAttr: true}, 8},
		{FileInfo_t{Fh: 44, PurgeUBC: true}, 16},
		{FileInfo_t{Fh: 45, PurgeAttr: true, PurgeUBC: true}, 24},
		{FileInfo_t{Fh: 46, DirectIo: true, KeepCache: true, NonSeekable: true, PurgeAttr: true, PurgeUBC: true}, 31},
	} {
		fh, mask := fileInfoBridgeProbe(test.info)
		want := test.mask
		if runtime.GOOS != "darwin" {
			want &= 7
		}
		if fh != test.info.Fh || mask != want {
			t.Fatalf("native bridge fh=%d flags=%d; want fh=%d flags=%d", fh, mask, test.info.Fh, want)
		}
	}
}
