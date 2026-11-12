package ui

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

type uploadCopyBackend struct {
	*fakeBackend
	mount model.MountID
	job   model.JobID
	calls int
}

func (b *uploadCopyBackend) UploadLocalCopy(_ context.Context, m model.MountID, j model.JobID) (manager.UploadCopy, error) {
	b.calls++
	if m != b.mount || j != b.job {
		return manager.UploadCopy{}, manager.ErrConfig
	}
	return manager.UploadCopy{Name: "手册.md", Content: []byte("<script>local draft</script>")}, nil
}
func TestUploadCopyDownloadsCapturedBytesAsAttachment(t *testing.T) {
	h, b, c, _, _ := fixture(t)
	extra := &uploadCopyBackend{fakeBackend: b, mount: model.MountID(uuid.NewString()), job: model.JobID(uuid.NewString())}
	h.backend = extra
	route := "/api/mounts/" + string(extra.mount) + "/uploads/" + string(extra.job) + "/local"
	res := request(t, c, h, "GET", route, nil, "", "")
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(raw) != "<script>local draft</script>" {
		t.Fatal(res.Status, string(raw))
	}
	if res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatal("unsafe inline content", res.Header)
	}
	res = request(t, c, h, "POST", route, map[string]string{}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("download bypassed session/CSRF guard", res.Status)
	}
	if extra.calls != 1 {
		t.Fatal("invalid request reached captured content")
	}
}
