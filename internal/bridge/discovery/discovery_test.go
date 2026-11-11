package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

var binding = Binding{ProfileID: "profile", IdentityHash: "identity", UserID: "123456", Origin: "https://tenant.feishu.cn"}

type fetchFunc func(context.Context, string, url.Values) ([]byte, error)

func (f fetchFunc) Get(c context.Context, p string, q url.Values) ([]byte, error) { return f(c, p, q) }
func page(tokens []string, more bool, cursor string, owner string) []byte {
	nodes := map[string]any{}
	for _, token := range tokens {
		nodes[token] = map[string]any{"token": token, "obj_token": token, "name": "name-" + token, "owner_id": owner, "type": 0}
	}
	b, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"entities": map[string]any{"nodes": nodes}, "node_list": tokens, "has_more": more, "last_label": cursor}})
	return b
}
func source(p string, q url.Values) string {
	if strings.Contains(p, "my_space") {
		return "personal"
	}
	if q.Get("hidden") == "0" {
		return "shared_visible"
	}
	return "shared_hidden"
}
func emptyFetcher(c context.Context, p string, q url.Values) ([]byte, error) {
	if source(p, q) == "personal" {
		return page([]string{"p"}, false, "", binding.UserID), nil
	}
	return page([]string{}, false, "", ""), nil
}

func TestScanPaginationAndMemberships(t *testing.T) {
	counts := map[string][]int{"personal": {1, 1, 1}, "shared_visible": {1, 1, 1}, "shared_hidden": {3, 3, 1}}
	positions := map[string]int{}
	f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
		s := source(p, q)
		i := positions[s]
		positions[s]++
		if q.Get("length") != "50" {
			t.Fatalf("length: %v", q)
		}
		asc, rank := "1", "5"
		if s == "shared_visible" {
			asc, rank = "0", "3"
		}
		if q.Get("asc") != asc || q.Get("rank") != rank {
			t.Fatalf("ordering %s: %v", s, q)
		}
		if i > 0 && q.Get("last_label") != fmt.Sprint(i) {
			t.Fatalf("cursor %s: %v", s, q)
		}
		tokens := []string{}
		for j := 0; j < counts[s][i]; j++ {
			tokens = append(tokens, fmt.Sprintf("%s%d%d", s, i, j))
		}
		if s == "shared_visible" && i == 0 {
			tokens[0] = "personal00"
		}
		return page(tokens, i < 2, fmt.Sprint(i+1), binding.UserID), nil
	})
	s, err := Scan(context.Background(), binding, f, Snapshot{})
	if err != nil || !s.Complete || len(s.Entries) != 12 || s.ScannedAt.IsZero() {
		t.Fatalf("scan: %+v %v", s, err)
	}
	for name, n := range map[string]int{"personal": 3, "shared_visible": 3, "shared_hidden": 7} {
		if s.Sources[name].Count != n || s.Sources[name].Pages != 3 || s.Sources[name].Status != "complete" {
			t.Fatalf("source %s: %+v", name, s.Sources[name])
		}
	}
	if !reflect.DeepEqual(s.Entries[0].Memberships, []string{"personal", "shared_visible"}) {
		t.Fatalf("dedup memberships: %+v", s.Entries[0])
	}
}

func TestIdentityGatesFreshSharedEntries(t *testing.T) {
	for _, test := range []struct {
		name, owner string
		empty       bool
	}{{"wrong owner", "999", false}, {"empty personal", "", true}} {
		t.Run(test.name, func(t *testing.T) {
			f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
				tokens := []string{"x"}
				if test.empty && source(p, q) == "personal" {
					tokens = []string{}
				}
				return page(tokens, false, "", test.owner), nil
			})
			s, err := Scan(context.Background(), binding, f, Snapshot{})
			if !errors.Is(err, ErrIdentity) || s.Complete || len(s.Entries) != 0 {
				t.Fatalf("identity not gated: %+v %v", s, err)
			}
		})
	}
	prior := Snapshot{Binding: binding, Sources: map[string]SourceState{"personal": {Status: "complete", Count: 1}}, Entries: []Entry{{Token: "old", Name: "old", Memberships: []string{"personal"}}}}
	f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
		return page([]string{}, false, "", ""), nil
	})
	s, err := Scan(context.Background(), binding, f, prior)
	if !errors.Is(err, ErrIdentity) || s.Complete || len(s.Entries) != 1 {
		t.Fatalf("prior snapshot replaced current identity proof: %+v %v", s, err)
	}
	other := prior
	other.Binding.UserID = "999"
	if _, err = Scan(context.Background(), binding, fetchFunc(emptyFetcher), other); !errors.Is(err, ErrIdentity) {
		t.Fatalf("cross identity previous: %v", err)
	}
}

func TestPartialRetainsPriorAndManualWithoutAliasing(t *testing.T) {
	prior := Snapshot{Binding: binding, Entries: []Entry{{Token: "old", Name: "old", Memberships: []string{"shared_hidden"}}, {Token: "manual", Name: "manual", Memberships: []string{"manual"}, Manual: true}}}
	f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
		if source(p, q) == "shared_hidden" {
			return nil, ErrNeedsLogin
		}
		return emptyFetcher(c, p, q)
	})
	s, err := Scan(context.Background(), binding, f, prior)
	if !errors.Is(err, ErrNeedsLogin) || s.Complete || s.Sources["shared_hidden"].Status != "needs_login" || len(s.Entries) != 3 {
		t.Fatalf("lost entries: %+v %v", s, err)
	}
	if strings.Contains(s.Sources["shared_hidden"].Error, "secret") {
		t.Fatal("raw message")
	}
	s.Entries[0].Memberships[0] = "changed"
	if prior.Entries[0].Memberships[0] != "shared_hidden" {
		t.Fatal("mutated prior")
	}
}

func TestProtocolFailures(t *testing.T) {
	cases := map[string][][]byte{
		"cycle":              {page([]string{"a"}, true, "same", binding.UserID), page([]string{"b"}, true, "same", binding.UserID)},
		"empty nonterminal":  {page([]string{}, true, "next", binding.UserID)},
		"empty continuation": {page([]string{"a"}, true, "next", binding.UserID), page([]string{}, false, "", binding.UserID)},
		"missing code":       {[]byte(`{"data":{}}`)},
		"missing fields":     {[]byte(`{"code":0,"data":{"node_list":[],"has_more":false}}`)},
		"invalid json":       {[]byte(`{"code":`)},
		"duplicate token":    {page([]string{"a"}, true, "next", binding.UserID), page([]string{"a"}, false, "", binding.UserID)},
		"missing node":       {[]byte(`{"code":0,"data":{"entities":{"nodes":{}},"node_list":["x"],"has_more":false,"last_label":""}}`)},
	}
	for name, pages := range cases {
		t.Run(name, func(t *testing.T) {
			i := 0
			f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
				if source(p, q) != "personal" {
					return page([]string{}, false, "", ""), nil
				}
				if i >= len(pages) {
					t.Fatal("unexpected extra page")
				}
				b := pages[i]
				i++
				return b, nil
			})
			s, err := Scan(context.Background(), binding, f, Snapshot{})
			if !errors.Is(err, ErrProtocol) || s.Complete || s.Sources["personal"].Status != "partial" {
				t.Fatalf("not rejected: %+v %v", s, err)
			}
		})
	}
}

func TestManualKeepsTokenAndAllowsSameName(t *testing.T) {
	s := Snapshot{Binding: binding, Entries: []Entry{{Token: "same", Name: "before", Memberships: []string{"personal"}}}}
	out, err := AddManual(s, Entry{Token: "same", Name: "after"})
	if err != nil || len(out.Entries) != 1 || !out.Entries[0].Manual || !reflect.DeepEqual(out.Entries[0].Memberships, []string{"personal", "manual"}) {
		t.Fatalf("manual merge: %+v %v", out, err)
	}
	out, err = AddManual(out, Entry{Token: "other", Name: "after"})
	if err != nil || len(out.Entries) != 2 {
		t.Fatalf("same-name entries merged: %+v %v", out, err)
	}
	if s.Entries[0].Name != "before" || len(s.Entries[0].Memberships) != 1 {
		t.Fatal("mutated original")
	}
	for _, entry := range []Entry{{Token: "x", Name: ""}, {Token: "../x", Name: "x"}, {Token: "x", Name: "\x00"}} {
		if _, err = AddManual(s, entry); err == nil {
			t.Fatalf("invalid manual: %+v", entry)
		}
	}
}

type statusError int

func (e statusError) Error() string   { return "secret upstream text" }
func (e statusError) StatusCode() int { return int(e) }

func TestRetriesAreBoundedAndSanitized(t *testing.T) {
	for _, test := range []struct{ code, want int }{{503, 3}, {429, 3}, {400, 1}, {401, 1}} {
		t.Run(fmt.Sprint(test.code), func(t *testing.T) {
			calls := 0
			f := fetchFunc(func(context.Context, string, url.Values) ([]byte, error) { calls++; return nil, statusError(test.code) })
			s, err := Scan(context.Background(), binding, f, Snapshot{})
			if err == nil || calls != test.want || strings.Contains(err.Error(), "secret") || strings.Contains(s.Sources[Personal].Error, "secret") {
				t.Fatalf("calls=%d snapshot=%+v err=%v", calls, s, err)
			}
			if test.code == 401 && !errors.Is(err, ErrNeedsLogin) {
				t.Fatalf("login status: %v", err)
			}
		})
	}
	calls := 0
	f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
		if source(p, q) == Personal {
			calls++
			if calls < 3 {
				return nil, statusError(503)
			}
		}
		return emptyFetcher(c, p, q)
	})
	if s, err := Scan(context.Background(), binding, f, Snapshot{}); err != nil || !s.Complete || calls != 3 {
		t.Fatalf("retry recovery: %+v %v calls=%d", s, err, calls)
	}
}

func TestCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	f := fetchFunc(func(context.Context, string, url.Values) ([]byte, error) {
		calls++
		cancel()
		return nil, statusError(503)
	})
	s, err := Scan(ctx, binding, f, Snapshot{})
	if !errors.Is(err, context.Canceled) || calls != 1 || s.Complete {
		t.Fatalf("cancel: %+v %v calls=%d", s, err, calls)
	}
}

func TestStrictJSONAndNodeValidation(t *testing.T) {
	valid := string(page([]string{"a"}, false, "", binding.UserID))
	cases := map[string]string{
		"duplicate field": strings.Replace(valid, `"code":0`, `"code":0,"code":0`, 1),
		"wrong type":      strings.Replace(valid, `"has_more":false`, `"has_more":"false"`, 1),
		"token mismatch":  strings.Replace(valid, `"token":"a"`, `"token":"b"`, 1),
		"not a folder":    strings.Replace(valid, `"type":0`, `"type":1`, 1),
		"trailing data":   valid + ` {}`,
		"large body":      strings.Repeat(" ", maxBody+1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePage([]byte(body), true, binding.UserID); !errors.Is(err, ErrProtocol) {
				t.Fatalf("accepted %s: %v", name, err)
			}
		})
	}
	// Owner IDs may be integers, but must not lose precision through float64.
	body := strings.Replace(valid, `"owner_id":"123456"`, `"owner_id":123456`, 1)
	if _, err := parsePage([]byte(body), true, binding.UserID); err != nil {
		t.Fatalf("numeric owner: %v", err)
	}
}

func TestPageBudget(t *testing.T) {
	count := 0
	f := fetchFunc(func(context.Context, string, url.Values) ([]byte, error) {
		count++
		return page([]string{fmt.Sprintf("token%d", count)}, true, fmt.Sprint(count), binding.UserID), nil
	})
	s, err := Scan(context.Background(), binding, f, Snapshot{})
	// Shared sources run only after personal established identity, and each has
	// an independent page budget; none can run indefinitely.
	if !errors.Is(err, ErrProtocol) || count != 3*maxPages || s.Complete {
		t.Fatalf("budget count=%d err=%v", count, err)
	}
}

func TestConflictingNamesForSameTokenAcrossSources(t *testing.T) {
	f := fetchFunc(func(c context.Context, p string, q url.Values) ([]byte, error) {
		b := page([]string{"a"}, false, "", binding.UserID)
		if source(p, q) == SharedVisible {
			b = []byte(strings.ReplaceAll(string(b), "name-a", "changed"))
		}
		return b, nil
	})
	s, err := Scan(context.Background(), binding, f, Snapshot{})
	if !errors.Is(err, ErrProtocol) || s.Complete || s.Sources[SharedVisible].Status != "partial" {
		t.Fatalf("inconsistent token accepted: %+v %v", s, err)
	}
}

func objectIdentityPage(tokens []string, nodes map[string]any, more bool, cursor string) []byte {
	body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"entities": map[string]any{"nodes": nodes}, "node_list": tokens, "has_more": more, "last_label": cursor}})
	return body
}
func TestEmptyPersonalFoldersVerifyCurrentOwnedObjects(t *testing.T) {
	calls := []string{}
	f := fetchFunc(func(_ context.Context, p string, q url.Values) ([]byte, error) {
		calls = append(calls, p+"?"+q.Encode())
		if strings.HasSuffix(p, "/my_space/folder/") {
			return page([]string{}, false, "", ""), nil
		}
		if strings.HasSuffix(p, "/my_space/obj/") {
			if q.Get("asc") != "1" || q.Get("rank") != "5" || q.Get("length") != "50" || q.Get("last_label") != "" {
				t.Fatal(q)
			}
			return objectIdentityPage([]string{"ownfile", "owndoc"}, map[string]any{
				"ownfile": map[string]any{"token": "ownfile", "obj_token": "differentObjectToken", "name": "File", "owner_id": binding.UserID, "type": 8},
				"owndoc":  map[string]any{"token": "owndoc", "obj_token": "differentDocToken", "name": "Doc", "owner_id": 123456, "type": 12},
				// The live response enriches nodes with resources outside node_list.
				"foreignEnriched": map[string]any{"token": "foreignEnriched", "owner_id": "999", "type": nil},
			}, true, "next"), nil
		}
		return page([]string{"shared"}, false, "", "999"), nil
	})
	s, err := Scan(context.Background(), binding, f, Snapshot{})
	if err != nil || !s.Complete || len(s.Entries) != 1 || s.Entries[0].Token != "shared" || s.Sources[Personal].Count != 0 || len(calls) != 4 {
		t.Fatalf("snapshot=%+v calls=%v err=%v", s, calls, err)
	}
	if !reflect.DeepEqual(s.Entries[0].Memberships, []string{SharedVisible, SharedHidden}) {
		t.Fatal(s.Entries)
	}
}
func TestEmptyPersonalCannotReusePreviousIdentityForSharedDiscovery(t *testing.T) {
	prior := Snapshot{Binding: binding, Sources: map[string]SourceState{Personal: {Count: 1, Status: "complete"}}, Entries: []Entry{{Token: "old", Name: "Old", Memberships: []string{Personal}}, {Token: "oldshared", Name: "Shared", Memberships: []string{SharedVisible}}}}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"empty", page([]string{}, false, "", "")},
		{"wrong owner", objectIdentityPage([]string{"foreign"}, map[string]any{"foreign": map[string]any{"token": "foreign", "name": "Foreign", "owner_id": "999", "type": 8}}, false, "")},
		{"mixed owners", objectIdentityPage([]string{"own", "foreign"}, map[string]any{"own": map[string]any{"token": "own", "name": "Own", "owner_id": binding.UserID, "type": 12}, "foreign": map[string]any{"token": "foreign", "name": "Foreign", "owner_id": "999", "type": 8}}, false, "")},
	} {
		for _, previous := range []Snapshot{{}, prior} {
			t.Run(fmt.Sprintf("%s/prior=%t", test.name, !emptySnapshot(previous)), func(t *testing.T) {
				sharedCalls := 0
				f := fetchFunc(func(_ context.Context, p string, q url.Values) ([]byte, error) {
					if strings.HasSuffix(p, "/my_space/folder/") {
						return page([]string{}, false, "", ""), nil
					}
					if strings.HasSuffix(p, "/my_space/obj/") {
						return test.body, nil
					}
					sharedCalls++
					return page([]string{"newshared"}, false, "", "999"), nil
				})
				s, err := Scan(context.Background(), binding, f, previous)
				if !errors.Is(err, ErrIdentity) || s.Complete || sharedCalls != 0 || !reflect.DeepEqual(s.Entries, sortedEntriesMust(previous.Entries)) {
					t.Fatalf("snapshot=%+v shared=%d err=%v", s, sharedCalls, err)
				}
			})
		}
	}
}
func sortedEntriesMust(entries []Entry) []Entry {
	m, err := entryMap(entries)
	if err != nil {
		panic(err)
	}
	return sortedEntries(m)
}
func TestObjectIdentityRejectsNullMalformedAndInvalidPagination(t *testing.T) {
	valid := string(objectIdentityPage([]string{"own"}, map[string]any{"own": map[string]any{"token": "own", "obj_token": "different", "name": "Own", "owner_id": binding.UserID, "type": 8}}, false, ""))
	cases := map[string][]byte{
		"null envelope":              []byte(`null`),
		"null data":                  []byte(`{"code":0,"data":null}`),
		"null node":                  []byte(`{"code":0,"data":{"entities":{"nodes":{"own":null}},"node_list":["own"],"has_more":false,"last_label":""}}`),
		"null owner":                 []byte(strings.Replace(valid, `"owner_id":"123456"`, `"owner_id":null`, 1)),
		"missing owner":              []byte(strings.Replace(valid, `"owner_id":"123456",`, ``, 1)),
		"fraction owner":             []byte(strings.Replace(valid, `"owner_id":"123456"`, `"owner_id":123456.0`, 1)),
		"null type":                  []byte(strings.Replace(valid, `"type":8`, `"type":null`, 1)),
		"fraction type":              []byte(strings.Replace(valid, `"type":8`, `"type":8.1`, 1)),
		"null node list":             []byte(strings.Replace(valid, `"node_list":["own"]`, `"node_list":null`, 1)),
		"null pagination":            []byte(strings.Replace(valid, `"has_more":false`, `"has_more":null`, 1)),
		"missing cursor":             []byte(strings.Replace(valid, `"last_label":"",`, ``, 1)),
		"nonterminal missing cursor": []byte(strings.Replace(valid, `"has_more":false`, `"has_more":true`, 1)),
		"empty nonterminal":          page([]string{}, true, "next", binding.UserID),
		"folder object mismatch":     []byte(strings.Replace(valid, `"type":8`, `"type":0`, 1)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := fetchFunc(func(_ context.Context, p string, _ url.Values) ([]byte, error) {
				if strings.HasSuffix(p, "/my_space/folder/") {
					return page([]string{}, false, "", ""), nil
				}
				if !strings.HasSuffix(p, "/my_space/obj/") {
					t.Fatal("shared discovery was not gated", p)
				}
				return body, nil
			})
			s, err := Scan(context.Background(), binding, f, Snapshot{})
			if !errors.Is(err, ErrProtocol) || s.Complete || len(s.Entries) != 0 {
				t.Fatalf("snapshot=%+v err=%v", s, err)
			}
		})
	}
}
