package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBackgroundPullExtensionDefaultsAndNormalization(t *testing.T) {
	tests := []struct {
		name  string
		mount Mount
		want  []string
	}{
		{"old configuration", Mount{}, []string{".log", ".db"}},
		{"new mount", DefaultMount(), []string{".log", ".db"}},
		{"explicit empty", Mount{BackgroundPullExcludedExtensions: []string{}}, []string{}},
		{"normalized", Mount{BackgroundPullExcludedExtensions: []string{" LOG ", ".DB", "log", " .db ", "sqlite3", "TMP_FILE"}}, []string{".log", ".db", ".sqlite3", ".tmp_file"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveBackgroundPullExcludedExtensions(tc.mount)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%#v %v; want %#v", got, err, tc.want)
			}
		})
	}
	defaults := DefaultMount()
	if !reflect.DeepEqual(defaults.BackgroundPullExcludedExtensions, []string{".log", ".db"}) {
		t.Fatalf("new defaults %#v", defaults.BackgroundPullExcludedExtensions)
	}
	got, _ := EffectiveBackgroundPullExcludedExtensions(Mount{})
	got[0] = ".changed"
	again, _ := EffectiveBackgroundPullExcludedExtensions(Mount{})
	if again[0] != ".log" {
		t.Fatal("defaults share caller-owned slice")
	}
}
func TestBackgroundPullExtensionRejectsInvalidRules(t *testing.T) {
	for _, rule := range []string{"", " ", ".", "..", "*.log", ".l?g", "[db]", "dir/.db", `dir\db`, ".tar.gz", "db/log", "d b", ".db\x00", "db,log", ":db"} {
		t.Run(rule, func(t *testing.T) {
			m := Mount{BackgroundPullExcludedExtensions: []string{rule}}
			if _, err := EffectiveBackgroundPullExcludedExtensions(m); err == nil {
				t.Fatalf("invalid rule %q accepted", rule)
			}
			paths := testPaths(t)
			c := testConfig(paths)
			c.Mounts[0].BackgroundPullExcludedExtensions = m.BackgroundPullExcludedExtensions
			if err := Validate(c, paths); err == nil {
				t.Fatalf("invalid mount rule %q accepted", rule)
			}
		})
	}
}
func TestBackgroundPullMatchesFileExtensionOnly(t *testing.T) {
	rules := []string{".log", ".db"}
	for _, tc := range []struct {
		remote string
		want   bool
	}{
		{"app.log", true}, {"nested/state.DB", true}, {"folder.log/readme.txt", false}, {"folder.db/readme", false}, {"audit.log.txt", false}, {"trace.LOG", true}, {"db", false}, {"file.db-backup", false}, {".db", true},
	} {
		if got := BackgroundPullExcluded(tc.remote, rules); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.remote, got, tc.want)
		}
	}
	if BackgroundPullExcluded("trace.log", []string{}) {
		t.Fatal("empty policy excluded file")
	}
}
func TestBackgroundPullExplicitEmptyRulesSurvivePersistence(t *testing.T) {
	paths := testPaths(t)
	c := testConfig(paths)
	c.Mounts[0].BackgroundPullExcludedExtensions = []string{}
	if err := SaveWithPaths(paths, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithPaths(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Mounts[0].BackgroundPullExcludedExtensions == nil {
		t.Fatal("empty policy became legacy default")
	}
	data, err := json.Marshal(loaded.Mounts[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["background_pull_excluded_extensions"]) != "[]" {
		t.Fatalf("explicit empty policy missing: %s", data)
	}
	var old Mount
	if err = json.Unmarshal([]byte(`{"id":"old"}`), &old); err != nil {
		t.Fatal(err)
	}
	rules, err := EffectiveBackgroundPullExcludedExtensions(old)
	if err != nil || !reflect.DeepEqual(rules, []string{".log", ".db"}) {
		t.Fatalf("old rules %#v %v", rules, err)
	}
}
