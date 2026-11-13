package history_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/stretchr/testify/require"
)

func TestAllArchivedContentLivesInGit(t *testing.T) {
	ctx := context.Background()
	s, opts, _ := fixture(t)
	e, err := s.EnsureEntry(ctx, "payload.bin", model.KindFile)
	require.NoError(t, err)
	payload := "\x00binary\x01payload"
	_, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader(payload))
	require.NoError(t, err)
	r, err := history.Open(ctx, opts)
	require.NoError(t, err)
	_, err = r.Ingest(ctx, s)
	require.NoError(t, err)
	page, err := r.List(ctx, history.Query{EntryID: e.ID, Kind: "local_saved"})
	require.NoError(t, err)
	require.Len(t, page.Versions, 1)
	ref := page.Versions[0].Bodies["local"]
	require.NotEmpty(t, ref.OID, "binary history must be a reachable Git blob")
	require.Empty(t, ref.Asset, "new history must not depend on a second body store")
	files, err := os.ReadDir(filepath.Join(opts.Dir, "objects"))
	require.NoError(t, err)
	require.Empty(t, files, "Git text and binary imports must not keep duplicate raw bodies")
	reopened, err := history.Open(ctx, opts)
	require.NoError(t, err)
	body, err := reopened.Read(ctx, page.Versions[0].ID, "local")
	require.NoError(t, err)
	require.Equal(t, payload, string(body))
}

func TestLargeGitBodyStreamsBeyondCommandOutputLimit(t *testing.T) {
	ctx := context.Background()
	s, opts, e := fixture(t)
	const size = 33 << 20
	_, _, err := s.Stage(ctx, e.ID, nil, io.LimitReader(repeatedByte('x'), size))
	require.NoError(t, err)
	r, err := history.Open(ctx, opts)
	require.NoError(t, err)
	_, err = r.Ingest(ctx, s)
	require.NoError(t, err)
	page, err := r.List(ctx, history.Query{EntryID: e.ID, Kind: "local_saved"})
	require.NoError(t, err)
	require.Len(t, page.Versions, 1)
	require.NotEmpty(t, page.Versions[0].Bodies["local"].OID)
	stream, err := r.OpenBody(ctx, page.Versions[0].ID, "local")
	require.NoError(t, err)
	n, err := io.Copy(io.Discard, stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, int64(size), n)
}

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

type countingHistorySource struct {
	*store.Store
	reads int
}

type mediaHistorySource struct {
	*store.Store
	media model.BlobID
}

func (s mediaHistorySource) HistoryEvents(ctx context.Context, after uint64, limit int) ([]model.HistoryEvent, error) {
	events, err := s.Store.HistoryEvents(ctx, after, limit)
	for i := range events {
		events[i].Media = map[string]model.BlobID{"local:picture.png": s.media}
	}
	return events, err
}

func TestArchivedPicturesRemainReachableGitObjectsAfterGC(t *testing.T) {
	ctx := context.Background()
	s, opts, e := fixture(t)
	_, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("![picture](picture.png)"))
	require.NoError(t, err)
	image := "\x89PNG\x00synthetic-image-payload"
	asset, err := s.PutBlob(ctx, strings.NewReader(image))
	require.NoError(t, err)
	r, err := history.Open(ctx, opts)
	require.NoError(t, err)
	_, err = r.Ingest(ctx, mediaHistorySource{Store: s, media: asset})
	require.NoError(t, err)
	page, err := r.List(ctx, history.Query{EntryID: e.ID, Kind: "local_saved"})
	require.NoError(t, err)
	require.Len(t, page.Versions, 1)
	require.NotEmpty(t, page.Versions[0].Media["local:picture.png"].OID)
	executable := opts.GitExecutable
	if executable == "" {
		executable = "git"
	}
	out, err := exec.CommandContext(ctx, executable, "--git-dir="+filepath.Join(opts.Dir, "history.git"), "gc", "--prune=now").CombinedOutput()
	require.NoError(t, err, string(out))
	stream, err := r.OpenBody(ctx, page.Versions[0].ID, "media:local:picture.png")
	require.NoError(t, err)
	b, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, image, string(b))
}

func (s *countingHistorySource) ReadBlob(ctx context.Context, id model.BlobID) (io.ReadCloser, error) {
	s.reads++
	return s.Store.ReadBlob(ctx, id)
}

func TestArchiveReusesGitContentWithoutReadingSourceAgain(t *testing.T) {
	ctx := context.Background()
	s, opts, e := fixture(t)
	source := &countingHistorySource{Store: s}
	_, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("same saved content"))
	require.NoError(t, err)
	r, err := history.Open(ctx, opts)
	require.NoError(t, err)
	_, err = r.Ingest(ctx, source)
	require.NoError(t, err)
	require.Positive(t, source.reads)
	source.reads = 0
	copy, err := s.EnsureEntry(ctx, "copy.md", model.KindFile)
	require.NoError(t, err)
	_, _, err = s.Stage(ctx, copy.ID, nil, strings.NewReader("same saved content"))
	require.NoError(t, err)
	result, err := r.Ingest(ctx, source)
	require.NoError(t, err)
	require.Positive(t, result.Archived)
	require.Zero(t, source.reads, "an already archived body should be reused from Git")
}
