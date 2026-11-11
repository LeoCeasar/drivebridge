package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type countedPullSource struct {
	Source
	files, docs int
}

func (s *countedPullSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	s.files++
	return s.Source.ObserveFile(ctx, token)
}
func (s *countedPullSource) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	s.docs++
	return s.Source.FetchDoc(ctx, token, revision)
}

func TestBackgroundPullPolicySkipsBothBodyFetchRoutes(t *testing.T) {
	for _, kind := range []model.Kind{model.KindFile, model.KindDocx} {
		t.Run(string(kind), func(t *testing.T) {
			s, engine, source, en := setup(t, kind)
			counted := &countedPullSource{Source: source}
			engine.source = counted
			allowed := false
			engine.deps.ShouldRefresh = func(context.Context, model.EntryID) (bool, error) { return allowed, nil }
			before := en.RemoteCheckpoint
			source.file = []byte("changed remotely")
			source.xml = `<title id="doc">Title</title><p id="a">changed remotely</p>`
			source.revision++
			if err := engine.RefreshEntry(bg, en.ID); err != nil {
				t.Fatal(err)
			}
			if counted.files != 0 || counted.docs != 0 {
				t.Fatalf("excluded refresh fetched bodies: files=%d docs=%d", counted.files, counted.docs)
			}
			after, err := s.Entry(bg, en.ID)
			if err != nil || after.RemoteCheckpoint != before {
				t.Fatalf("excluded refresh changed checkpoint: %+v %v", after, err)
			}
			// A live policy change takes effect without rebuilding the engine.
			allowed = true
			if err = engine.RefreshEntry(bg, en.ID); err != nil {
				t.Fatal(err)
			}
			if counted.files+counted.docs != 1 {
				t.Fatalf("allowed refresh did not fetch body: %+v", counted)
			}
			after, err = s.Entry(bg, en.ID)
			if err != nil || after.RemoteCheckpoint == before {
				t.Fatalf("allowed refresh did not publish: %+v %v", after, err)
			}
		})
	}
}
func TestBackgroundPullPolicyErrorPreventsFetch(t *testing.T) {
	_, engine, source, en := setup(t, model.KindFile)
	counted := &countedPullSource{Source: source}
	engine.source = counted
	denied := errors.New("policy unavailable")
	engine.deps.ShouldRefresh = func(context.Context, model.EntryID) (bool, error) { return false, denied }
	if err := engine.RefreshEntry(bg, en.ID); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if counted.files != 0 || counted.docs != 0 {
		t.Fatal("fetched after policy failure")
	}
}
func TestExcludedBackgroundPullStillUploadsLocalChanges(t *testing.T) {
	for _, isNew := range []bool{false, true} {
		name := "modify"
		if isNew {
			name = "create"
		}
		t.Run(name, func(t *testing.T) {
			s, engine, source, _ := setup(t, model.KindFile)
			en, err := s.EnsureEntry(bg, "application.LOG", model.KindFile)
			if err != nil {
				t.Fatal(err)
			}
			if !isNew {
				en, err = s.ObserveEntry(bg, store.Observation{Remote: en.Remote, Kind: en.Kind, Token: "log-resource"})
				if err != nil {
					t.Fatal(err)
				}
				sn, er := engine.observeFile(bg, en.Token)
				if er != nil {
					t.Fatal(er)
				}
				en, err = s.AdoptFileSnapshot(bg, en.ID, en.Generation, sn.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			rules, err := config.EffectiveBackgroundPullExcludedExtensions(config.DefaultMount())
			if err != nil {
				t.Fatal(err)
			}
			counted := &countedPullSource{Source: source}
			engine.source = counted
			policyCalls := 0
			engine.deps.ShouldRefresh = func(ctx context.Context, id model.EntryID) (bool, error) {
				policyCalls++
				e, err := s.Entry(ctx, id)
				return !config.BackgroundPullExcluded(e.Remote, rules), err
			}
			if err = engine.RefreshEntry(bg, en.ID); err != nil {
				t.Fatal(err)
			}
			if counted.files != 0 {
				t.Fatal("excluded local log downloaded by refresh")
			}
			en = stageContent(t, s, en, "locally changed log")
			if err = engine.SyncEntry(bg, en.ID); err != nil {
				t.Fatal(err)
			}
			if source.writes != 1 || string(source.file) != "locally changed log" {
				t.Fatalf("local log did not upload: writes=%d body=%q", source.writes, source.file)
			}
			after, err := s.Entry(bg, en.ID)
			if err != nil || after.Status != model.StatusSynced {
				t.Fatalf("upload not confirmed: %+v %v", after, err)
			}
			if counted.files == 0 {
				t.Fatal("upload readback was excluded")
			}
			if !isNew && policyCalls != 1 {
				t.Fatalf("upload unexpectedly consulted background policy %d times", policyCalls)
			}
			if err = s.Validate(bg); err != nil {
				t.Fatal(err)
			}
		})
	}
}
