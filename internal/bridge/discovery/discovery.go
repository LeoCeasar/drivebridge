// Package discovery reads the web application's directory entrance lists.
// Completeness describes these lists, not every folder accessible by link.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrNeedsLogin          = errors.New("directory discovery requires web login")
	ErrIdentity            = errors.New("directory discovery identity is not verified")
	ErrProtocol            = errors.New("unsupported directory discovery response")
	ErrBrowserDisconnected = errors.New("directory discovery browser is disconnected")
	errRequest             = errors.New("directory request failed")
)

const (
	Personal      = "personal"
	SharedVisible = "shared_visible"
	SharedHidden  = "shared_hidden"
	Manual        = "manual"
	maxPages      = 1000
	maxBody       = 16 << 20
)

type Binding struct {
	ProfileID    string `json:"profile_id"`
	IdentityHash string `json:"identity_hash"`
	UserID       string `json:"user_id"`
	Origin       string `json:"origin"`
}
type Entry struct {
	Token       string   `json:"token"`
	Name        string   `json:"name"`
	Memberships []string `json:"memberships"`
	Manual      bool     `json:"manual"`
	// Placement is independently verified namespace ownership. Memberships
	// continue to describe discovery provenance, including manual additions.
	Placement          string   `json:"placement,omitempty"`
	CanonicalRootToken string   `json:"canonical_root_token,omitempty"`
	RelativePath       []string `json:"relative_path,omitempty"`
}
type SourceState struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
	Pages  int    `json:"pages"`
	Error  string `json:"error,omitempty"`
}
type Snapshot struct {
	Binding   Binding                `json:"binding"`
	ScannedAt time.Time              `json:"scanned_at"`
	Entries   []Entry                `json:"entries"`
	Sources   map[string]SourceState `json:"sources"`
	Complete  bool                   `json:"complete"`
}
type Fetcher interface {
	Get(context.Context, string, url.Values) ([]byte, error)
}

type sourceSpec struct{ name, path, asc, rank, hidden string }

var sources = []sourceSpec{
	{Personal, "/space/api/explorer/v3/my_space/folder/", "1", "5", ""},
	{SharedVisible, "/space/api/explorer/v2/share/folder/list/", "0", "3", "0"},
	{SharedHidden, "/space/api/explorer/v2/share/folder/list/", "1", "5", "1"},
}

// Scan preserves prior entries even when a list no longer contains them: a
// missing entrance is not proof of deletion or revoked access. The caller must
// load previous only from its trusted per-binding cache. Identity always requires
// current personal evidence; historical entries never verify the current browser.
// An empty folder list falls back to current personal objects for owner proof.
func Scan(ctx context.Context, b Binding, f Fetcher, previous Snapshot) (Snapshot, error) {
	out := Snapshot{Binding: b, ScannedAt: time.Now().UTC(), Entries: []Entry{}, Sources: map[string]SourceState{}}
	if !validBinding(b) || (!emptySnapshot(previous) && previous.Binding != b) {
		return out, ErrIdentity
	}
	if f == nil {
		return out, ErrProtocol
	}
	entries, err := entryMap(previous.Entries)
	if err != nil {
		return out, err
	}
	verified := false
	var failures []error
	observedNames := map[string]string{}
	for _, spec := range sources {
		if spec.name != Personal && !verified {
			out.Sources[spec.name] = SourceState{Status: "partial", Error: ErrIdentity.Error()}
			failures = append(failures, fmt.Errorf("%s: %w", spec.name, ErrIdentity))
			continue
		}
		found, state, sourceErr := scanSource(ctx, b, f, spec)
		// An owner mismatch invalidates this session even when older evidence exists.
		if spec.name == Personal {
			if errors.Is(sourceErr, ErrIdentity) {
				verified = false
			} else if len(found) > 0 {
				verified = true
			}
			if sourceErr == nil && len(found) == 0 {
				sourceErr = verifyPersonalObjects(ctx, b, f)
				verified = sourceErr == nil
			}
		}
		// A token is the resource identity. Conflicting names in the same scan
		// indicate an unstable inventory and must not silently overwrite a source.
		for _, e := range found {
			if name, ok := observedNames[e.Token]; ok && name != e.Name {
				if sourceErr == nil {
					sourceErr = ErrProtocol
				}
				found = nil
				break
			}
		}
		if sourceErr != nil {
			state.Status = "partial"
			if errors.Is(sourceErr, ErrNeedsLogin) {
				state.Status = "needs_login"
			}
			state.Error = sourceErr.Error()
			failures = append(failures, fmt.Errorf("%s: %w", spec.name, sourceErr))
		} else {
			state.Status = "complete"
		}
		// Identity failure never adds unverified personal or shared data. Other
		// failures may retain already validated pages, alongside the old inventory.
		if verified {
			for _, e := range found {
				observedNames[e.Token] = e.Name
				merge(entries, e)
			}
		}
		out.Sources[spec.name] = state
	}
	out.Entries = sortedEntries(entries)
	out.Complete = len(failures) == 0
	return out, errors.Join(failures...)
}

// AddManual marks a caller-verified folder as a manual entrance. Its identity
// is the snapshot's binding; callers must verify the folder using that account
// before invoking this function. Equal names with different tokens stay distinct.
func AddManual(s Snapshot, entry Entry) (Snapshot, error) {
	entry.Placement = ""
	entry.CanonicalRootToken = ""
	entry.RelativePath = nil
	return addManual(s, entry, false)
}

// AddVerifiedManual accepts placement proven by current account metadata. A
// covered child refers to an existing entrance; it must not become another root.
func AddVerifiedManual(s Snapshot, entry Entry) (Snapshot, error) {
	if entry.Placement != "personal" && entry.Placement != "shared" {
		return s, ErrProtocol
	}
	return addManual(s, entry, true)
}
func addManual(s Snapshot, entry Entry, verified bool) (Snapshot, error) {
	if !validBinding(s.Binding) {
		return s, ErrIdentity
	}
	if !validEntry(entry) {
		return s, ErrProtocol
	}
	entries, err := entryMap(s.Entries)
	if err != nil {
		return s, err
	}
	if verified && entry.CanonicalRootToken != "" && entry.CanonicalRootToken != entry.Token {
		anchor, ok := entries[entry.CanonicalRootToken]
		if !ok || (anchor.CanonicalRootToken != "" && anchor.CanonicalRootToken != anchor.Token) {
			return s, ErrProtocol
		}
	}
	// Provenance stays manual; placement must not impersonate a web shared list.
	entry.Memberships = []string{Manual}
	if verified && entry.Placement == Personal {
		entry.Memberships = append(entry.Memberships, Personal)
	}
	entry.Manual = true
	merge(entries, entry)
	out := s
	out.Entries = sortedEntries(entries)
	out.Sources = make(map[string]SourceState, len(s.Sources))
	for k, v := range s.Sources {
		out.Sources[k] = v
	}
	return out, nil
}

func scanSource(ctx context.Context, b Binding, f Fetcher, spec sourceSpec) ([]Entry, SourceState, error) {
	state := SourceState{}
	out := []Entry{}
	seenTokens := map[string]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	for i := 0; i < maxPages; i++ {
		q := url.Values{"length": {"50"}, "asc": {spec.asc}, "rank": {spec.rank}}
		if spec.hidden != "" {
			q.Set("hidden", spec.hidden)
		}
		if cursor != "" {
			q.Set("last_label", cursor)
		}
		body, err := fetch(ctx, f, spec.path, q)
		if err != nil {
			return out, state, err
		}
		page, err := parsePage(body, spec.name == Personal, b.UserID)
		if err != nil {
			return out, state, err
		}
		state.Pages++
		// Empty continuation pages previously hid known shared entries in practice.
		// Treat these as partial even if the server now claims a terminal page.
		if len(page.entries) == 0 && (page.more || i > 0) {
			return out, state, ErrProtocol
		}
		for _, e := range page.entries {
			if seenTokens[e.Token] {
				return out, state, ErrProtocol
			}
			seenTokens[e.Token] = true
		}
		if page.more && (page.cursor == "" || seenCursors[page.cursor]) {
			return out, state, ErrProtocol
		}
		for _, e := range page.entries {
			e.Memberships = []string{spec.name}
			out = append(out, e)
		}
		state.Count = len(out)
		if !page.more {
			return out, state, nil
		}
		seenCursors[page.cursor] = true
		cursor = page.cursor
	}
	return out, state, ErrProtocol
}

// Empty folder roots may still contain personal files. Only the selected first
// object page establishes the current browser owner's identity; its resources
// never enter the folder catalog, and enriched nodes outside node_list are ignored.
func verifyPersonalObjects(ctx context.Context, b Binding, f Fetcher) error {
	body, err := fetch(ctx, f, "/space/api/explorer/v3/my_space/obj/", url.Values{"asc": {"1"}, "rank": {"5"}, "length": {"50"}})
	if err != nil {
		return err
	}
	page, err := parseResourcePage(body, true, b.UserID, false)
	if err != nil {
		return err
	}
	if page.more && (page.cursor == "" || len(page.entries) == 0) {
		return ErrProtocol
	}
	if len(page.entries) == 0 {
		return ErrIdentity
	}
	return nil
}

type responsePage struct {
	entries []Entry
	more    bool
	cursor  string
}

func parsePage(body []byte, personal bool, userID string) (responsePage, error) {
	return parseResourcePage(body, personal, userID, true)
}

func parseResourcePage(body []byte, personal bool, userID string, foldersOnly bool) (responsePage, error) {
	bad := func() (responsePage, error) { return responsePage{}, ErrProtocol }
	if len(body) > maxBody || !utf8.Valid(body) {
		return bad()
	}
	// Reject duplicate JSON object keys and pathological nesting, while permitting
	// the many unrelated metadata fields supplied by the upstream web app.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := checkJSON(dec, 0); err != nil {
		return bad()
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad()
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil {
		return bad()
	}
	if *envelope.Code != 0 {
		if *envelope.Code == 99991663 || *envelope.Code == 99991661 || *envelope.Code == 99991668 {
			return responsePage{}, ErrNeedsLogin
		}
		return bad()
	}
	var data struct {
		Entities *struct {
			Nodes map[string]json.RawMessage `json:"nodes"`
		} `json:"entities"`
		Tokens *[]string `json:"node_list"`
		More   *bool     `json:"has_more"`
		Cursor *string   `json:"last_label"`
	}
	if json.Unmarshal(envelope.Data, &data) != nil || data.Entities == nil || data.Entities.Nodes == nil || data.Tokens == nil || data.More == nil || data.Cursor == nil {
		return bad()
	}
	out := responsePage{more: *data.More, cursor: *data.Cursor, entries: []Entry{}}
	seen := map[string]bool{}
	for _, token := range *data.Tokens {
		if seen[token] {
			return bad()
		}
		seen[token] = true
		var n struct {
			Token       string          `json:"token"`
			ObjectToken string          `json:"obj_token"`
			Name        string          `json:"name"`
			Owner       json.RawMessage `json:"owner_id"`
			Type        *int            `json:"type"`
		}
		raw, ok := data.Entities.Nodes[token]
		if !ok || json.Unmarshal(raw, &n) != nil || n.Token != token || n.Type == nil {
			return bad()
		}
		if (foldersOnly && *n.Type != 0) || ((foldersOnly || *n.Type == 0) && n.ObjectToken != "" && n.ObjectToken != token) {
			return bad()
		}
		e := Entry{Token: token, Name: n.Name}
		if !validEntry(e) {
			return bad()
		}
		if personal {
			owner, err := parseOwnerID(n.Owner)
			if err != nil {
				return bad()
			}
			if owner != userID {
				return responsePage{}, ErrIdentity
			}
		}
		out.entries = append(out.entries, e)
	}
	return out, nil
}

func parseOwnerID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", ErrProtocol
	}
	var owner string
	if json.Unmarshal(raw, &owner) != nil {
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return "", ErrProtocol
		}
		owner = number.String()
	}
	if owner == "" {
		return "", ErrProtocol
	}
	for _, r := range owner {
		if r < '0' || r > '9' {
			return "", ErrProtocol
		}
	}
	return owner, nil
}

func checkJSON(d *json.Decoder, depth int) error {
	if depth > 128 {
		return ErrProtocol
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return ErrProtocol
			}
			keys[name] = true
			if err = checkJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = checkJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return ErrProtocol
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	want := json.Delim('}')
	if delim == '[' {
		want = ']'
	}
	if end != want {
		return ErrProtocol
	}
	return nil
}

func fetch(ctx context.Context, f Fetcher, path string, q url.Values) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values := url.Values{}
		for k, v := range q {
			values[k] = append([]string(nil), v...)
		}
		body, err := f.Get(ctx, path, values)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			if len(body) > maxBody {
				return nil, ErrProtocol
			}
			return body, nil
		}
		if errors.Is(err, ErrNeedsLogin) {
			return nil, ErrNeedsLogin
		}
		if errors.Is(err, ErrBrowserDisconnected) {
			return nil, ErrBrowserDisconnected
		}
		if errors.Is(err, ErrProtocol) {
			return nil, ErrProtocol
		}
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		transient := false
		var status interface{ StatusCode() int }
		if errors.As(err, &status) {
			code := status.StatusCode()
			if code == 401 || code == 403 {
				return nil, ErrNeedsLogin
			}
			transient = code == 429 || (code >= 500 && code <= 599)
		}
		var network net.Error
		if errors.As(err, &network) {
			transient = transient || network.Timeout() || network.Temporary()
		}
		if !transient || attempt == 2 {
			return nil, errRequest
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errRequest
}

func validBinding(b Binding) bool {
	if strings.TrimSpace(b.ProfileID) == "" || strings.TrimSpace(b.IdentityHash) == "" || b.UserID == "" {
		return false
	}
	for _, r := range b.UserID {
		if r < '0' || r > '9' {
			return false
		}
	}
	u, err := url.Parse(b.Origin)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}
func validEntry(e Entry) bool {
	if e.Placement != "" && e.Placement != "personal" && e.Placement != "shared" {
		return false
	}
	if e.CanonicalRootToken != "" {
		if e.Placement == "" || len(e.CanonicalRootToken) > 256 {
			return false
		}
		for _, r := range e.CanonicalRootToken {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
				return false
			}
		}
		if e.CanonicalRootToken != e.Token && len(e.RelativePath) == 0 {
			return false
		}
		if e.CanonicalRootToken == e.Token && len(e.RelativePath) != 0 {
			return false
		}
	} else if len(e.RelativePath) != 0 {
		return false
	}
	if len(e.RelativePath) > 64 {
		return false
	}
	for _, part := range e.RelativePath {
		if strings.TrimSpace(part) == "" || len(part) > 4096 || !utf8.ValidString(part) {
			return false
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	if e.Token == "" || len(e.Token) > 256 || strings.TrimSpace(e.Name) == "" || !utf8.ValidString(e.Name) {
		return false
	}
	for _, r := range e.Token {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	for _, r := range e.Name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func emptySnapshot(s Snapshot) bool {
	return s.Binding == (Binding{}) && s.ScannedAt.IsZero() && len(s.Entries) == 0 && len(s.Sources) == 0 && !s.Complete
}
func entryMap(entries []Entry) (map[string]Entry, error) {
	out := map[string]Entry{}
	for _, e := range entries {
		if !validEntry(e) {
			return nil, ErrProtocol
		}
		if _, ok := out[e.Token]; ok {
			return nil, ErrProtocol
		}
		e.Memberships = append([]string(nil), e.Memberships...)
		e.RelativePath = append([]string(nil), e.RelativePath...)
		for _, m := range e.Memberships {
			if m != Personal && m != SharedVisible && m != SharedHidden && m != Manual {
				return nil, ErrProtocol
			}
		}
		if e.Manual && !has(e.Memberships, Manual) {
			e.Memberships = append(e.Memberships, Manual)
		}
		if has(e.Memberships, Manual) {
			e.Manual = true
		}
		out[e.Token] = e
	}
	return out, nil
}
func merge(entries map[string]Entry, e Entry) {
	old, ok := entries[e.Token]
	if !ok {
		old = Entry{Token: e.Token}
	}
	old.Name = e.Name
	old.Manual = old.Manual || e.Manual
	if e.Placement != "" {
		old.Placement = e.Placement
		old.CanonicalRootToken = e.CanonicalRootToken
		old.RelativePath = append([]string(nil), e.RelativePath...)
	}
	for _, m := range e.Memberships {
		if !has(old.Memberships, m) {
			old.Memberships = append(old.Memberships, m)
		}
	}
	entries[e.Token] = old
}
func has(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
func sortedEntries(entries map[string]Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	order := map[string]int{Personal: 0, SharedVisible: 1, SharedHidden: 2, Manual: 3}
	for _, e := range entries {
		sort.Slice(e.Memberships, func(i, j int) bool { return order[e.Memberships[i]] < order[e.Memberships[j]] })
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}
