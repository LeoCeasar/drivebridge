package manager

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

func (m *Manager) discoverySession(ctx context.Context, id model.ProfileID, origin string) (discoverySession, error) {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	if m.discoveryClosing || m.ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if m.discoverySessions == nil {
		m.discoverySessions = map[model.ProfileID]discoverySession{}
	}
	if session := m.discoverySessions[id]; session != nil {
		if native, ok := session.(interface{ Connected() bool }); !ok || native.Connected() {
			return session, nil
		}
		_ = session.Close()
		delete(m.discoverySessions, id)
	}
	var err error
	factory := m.discoveryFactory
	var session discoverySession
	if factory == nil {
		session, err = m.ownedDiscoverySession(m.ctx, id, origin)
	} else {
		session, err = factory(m.ctx, id, origin)
	}
	if err != nil {
		return nil, err
	}
	m.discoverySessions[id] = session
	return session, nil
}
func (m *Manager) dropDiscoverySession(id model.ProfileID) {
	m.discoveryMu.Lock()
	session := m.discoverySessions[id]
	delete(m.discoverySessions, id)
	m.discoveryMu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}
func (m *Manager) closeDiscoverySessions() {
	m.discoveryMu.Lock()
	m.discoveryClosing = true
	for _, cancel := range m.discoveryLogins {
		cancel()
	}
	m.discoveryLogins = nil
	sessions := m.discoverySessions
	m.discoverySessions = nil
	m.discoveryMu.Unlock()
	m.discoveryWG.Wait()
	for _, session := range sessions {
		_ = session.Close()
	}
}
func (m *Manager) StartDiscoveryLogin(ctx context.Context, id model.ProfileID, origin string) (DiscoveryView, error) {
	release, err := m.lockDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	defer release()
	profile, _, settings, err := m.verifiedDiscovery(ctx, id, strings.TrimSpace(origin))
	if err != nil {
		return DiscoveryView{}, err
	}
	_, snapshot, err := m.readDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if snapshot.Binding.ProfileID == "" {
		snapshot.Binding = settings.Binding
	}
	settings.Enabled = true
	settings.State = "login_pending"
	settings.Error = ""
	session, err := m.discoverySession(ctx, id, settings.Binding.Origin)
	if err == nil {
		err = session.Login(ctx)
	}
	if err != nil {
		settings.State = discoveryStateError(err)
		settings.Error = settings.State
		m.dropDiscoverySession(id)
	}
	if saveErr := m.writeDiscovery(ctx, id, settings, snapshot); saveErr != nil {
		return DiscoveryView{}, saveErr
	}
	view, viewErr := m.discoveryView(ctx, profile)
	if err != nil {
		return view, err
	}
	if viewErr == nil {
		m.watchDiscoveryLogin(id)
	}
	return view, viewErr
}
func (m *Manager) scanDiscovery(ctx context.Context, id model.ProfileID, completeLogin bool) (DiscoveryView, error) {
	release, err := m.lockDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	defer release()
	cached, previous, err := m.readDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if previous.Binding.ProfileID == "" {
		return DiscoveryView{}, discovery.ErrNeedsLogin
	}
	profile, _, settings, err := m.verifiedDiscovery(ctx, id, "")
	if err != nil {
		return m.failedDiscoveryCompletion(ctx, profile, cached, previous, err)
	}
	// Manual completion and the background watcher can arrive together. Once a
	// verified account mount is already ready, neither should seal/reopen login or
	// request another mount start.
	if completeLogin {
		view, viewErr := m.discoveryView(ctx, profile)
		if viewErr == nil && discoveryLoginReady(ctx, view, m.Status) {
			return view, nil
		}
	}
	_, previous, err = m.readDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if previous.Binding.ProfileID == "" {
		return DiscoveryView{}, discovery.ErrNeedsLogin
	}
	session, err := m.discoverySession(ctx, id, settings.Binding.Origin)
	if err != nil {
		settings.State = discoveryStateError(err)
		settings.Error = settings.State
		_ = m.writeDiscovery(ctx, id, settings, previous)
		view, _ := m.discoveryView(ctx, profile)
		return view, err
	}
	next := scanDiscoveryState(ctx, &settings, session, previous)
	if err = m.writeDiscovery(ctx, id, settings, next); err != nil {
		return DiscoveryView{}, err
	}
	// Keep the owned window alive so its verified completion page remains visible.
	// Normal rescans reuse the account's private persistent WebView session.
	if completeLogin && next.Complete {
		if err = session.SealLogin(ctx); err != nil {
			return m.failedDiscoveryCompletion(ctx, profile, settings, next, err)
		}
		mount, err := m.ensureAccountMount(ctx, profile)
		if err != nil {
			return m.failedDiscoveryCompletion(ctx, profile, settings, next, err)
		}
		if _, err = m.Start(ctx, mount.ID, true); err != nil {
			return m.failedDiscoveryCompletion(ctx, profile, settings, next, err)
		}
	}
	view, err := m.discoveryView(ctx, profile)
	if err == nil && view.MountID != "" {
		_ = m.call(ctx, view.MountID, "/refresh-directories", struct{}{}, nil)
	}
	return view, err
}
func (m *Manager) CompleteDiscoveryLogin(ctx context.Context, id model.ProfileID) (DiscoveryView, error) {
	return m.scanDiscovery(ctx, id, true)
}
func (m *Manager) RescanDiscovery(ctx context.Context, id model.ProfileID) (DiscoveryView, error) {
	return m.scanDiscovery(ctx, id, false)
}

func (m *Manager) SearchDiscoveryFolders(ctx context.Context, id model.ProfileID, query string) ([]DiscoveryCandidate, error) {
	query = strings.TrimSpace(query)
	if !discoveryName(query) || utf8.RuneCountInString(query) > 30 {
		return nil, ErrConfig
	}
	release, err := m.lockDiscovery(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	_, runner, settings, err := m.verifiedDiscovery(ctx, id, "")
	if err != nil {
		return nil, err
	}
	cursor := ""
	seen := map[string]bool{"": true}
	tokens := map[string]bool{}
	out := []DiscoveryCandidate{}
	for page := 0; page < 100; page++ {
		args := []string{"drive", "+search", "--query", query, "--doc-types", "folder", "--page-size", "20"}
		if cursor != "" {
			args = append(args, "--page-token", cursor)
		}
		response, err := runner.Run(ctx, larkcli.Request{Args: args})
		if err != nil {
			return nil, err
		}
		found, more, nextCursor, err := decodeDiscoverySearch(response.Data, settings.Binding.Origin)
		if err != nil {
			return nil, err
		}
		for _, token := range found {
			if tokens[token] {
				continue
			}
			folder, err := discoveryFolder(ctx, runner, token, settings.Binding.Origin)
			if err != nil {
				return nil, err
			}
			tokens[token] = true
			out = append(out, folder)
		}
		if !more {
			return out, nil
		}
		if len(found) == 0 || nextCursor == "" || seen[nextCursor] {
			return nil, discovery.ErrProtocol
		}
		seen[nextCursor] = true
		cursor = nextCursor
	}
	return nil, discovery.ErrProtocol
}
func (m *Manager) AddDiscoveryFolder(ctx context.Context, id model.ProfileID, input string) (DiscoveryView, error) {
	release, err := m.lockDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	defer release()
	_, prior, err := m.readDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if prior.Binding.ProfileID == "" {
		return DiscoveryView{}, discovery.ErrNeedsLogin
	}
	profile, runner, settings, err := m.verifiedDiscovery(ctx, id, "")
	if err != nil {
		return DiscoveryView{}, err
	}
	token, err := discoveryFolderToken(input, settings.Binding.Origin)
	if err != nil {
		return DiscoveryView{}, err
	}
	if token == settings.RootToken || token == accountToken(id) {
		return DiscoveryView{}, ErrConfig
	}
	folder, err := discoveryFolder(ctx, runner, token, settings.Binding.Origin)
	if err != nil {
		return DiscoveryView{}, err
	}
	_, snapshot, err := m.readDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if snapshot.Binding.ProfileID == "" {
		snapshot.Binding = settings.Binding
		settings.State = "idle"
	}
	classificationCtx, cancelClassification := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelClassification()
	placement, classifyErr := classifyManualFolder(classificationCtx, officialClassificationSource{runner: runner, drive: larkcli.NewDrive(runner, "")}, snapshot, folder)
	if classifyErr != nil {
		view, _ := m.discoveryView(ctx, profile)
		return view, classifyErr
	}
	if placement.CanonicalRootToken != folder.Token {
		snapshot, err = discovery.AddVerifiedManual(snapshot, discovery.Entry{Token: placement.CanonicalRootToken, Name: placement.RootName, Placement: placement.Placement, CanonicalRootToken: placement.CanonicalRootToken})
		if err != nil {
			return DiscoveryView{}, err
		}
	}
	snapshot, err = discovery.AddVerifiedManual(snapshot, discovery.Entry{Token: folder.Token, Name: folder.Name, Placement: placement.Placement, CanonicalRootToken: placement.CanonicalRootToken, RelativePath: placement.RelativePath})
	if err != nil {
		return DiscoveryView{}, err
	}
	if err = m.writeDiscovery(ctx, id, settings, snapshot); err != nil {
		return DiscoveryView{}, err
	}
	view, err := m.discoveryView(ctx, profile)
	if err == nil && view.MountID != "" {
		_ = m.call(ctx, view.MountID, "/refresh-directories", struct{}{}, nil)
	}
	return view, err
}

// RunDiscovery refreshes enabled catalogs without starting a stopped mount.
func (m *Manager) RunDiscovery(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-timer.C:
			views, err := m.DiscoveryViews(ctx)
			if err == nil {
				for _, view := range views {
					if !view.Configured || !view.Enabled || view.State == "login_pending" || view.State == "needs_login" || time.Since(view.LastScan) < 5*time.Minute {
						continue
					}
					scanCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					_, err = m.RescanDiscovery(scanCtx, view.ProfileID)
					cancel()
					if errors.Is(err, context.Canceled) {
						return
					}
				}
			}
			timer.Reset(5 * time.Minute)
		}
	}
}
