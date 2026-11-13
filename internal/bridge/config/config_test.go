package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/file"
)

const profileID = "11111111-1111-4111-8111-111111111111"
const remoteID = "22222222-2222-4222-8222-222222222222"
const mountID = "33333333-3333-4333-8333-333333333333"
const identityID = "44444444-4444-4444-8444-444444444444"

func testPaths(t *testing.T) Paths {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Paths{filepath.Join(root, "config", "config.json"), filepath.Join(root, "state"), filepath.Join(root, "cache")}
}
func testConfig(p Paths) Config {
	return Config{SchemaVersion: 1, Profiles: []Profile{{ID: profileID, Name: "work", Brand: "feishu"}}, Remotes: []Remote{{ID: remoteID, Name: "team", ProfileID: profileID, FolderToken: "selected-folder", NewMDPolicy: "docx"}}, Mounts: []Mount{{ID: mountID, Name: "team", RemoteID: remoteID, Mountpoint: filepath.Join(filepath.Dir(p.StateDir), "mount"), CacheMode: "full", CacheMaxBytes: 5368709120, WriteBack: "2s", PollDocs: "5s", RefreshDirs: "30s", MaxDocBytes: 10485760, Notify: true, MountEngine: "auto"}}}
}
func TestProfileIsolation(t *testing.T) {
	a, b := testPaths(t), testPaths(t)
	x, err := a.ProfileDir(profileID)
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.ProfileDir(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if x == y {
		t.Fatal("different users share profile state")
	}
	m, err := a.MountPaths(mountID)
	if err != nil {
		t.Fatal(err)
	}
	if m.VFS != filepath.Join(a.StateDir, "mounts", mountID, "vfs") || m.Database != filepath.Join(a.StateDir, "mounts", mountID, "state.db") || m.Cache != filepath.Join(a.CacheDir, mountID) {
		t.Fatalf("incorrect persistent mount layout: %+v", m)
	}
	if _, err := a.ProfileDir("../other"); err == nil {
		t.Fatal("path injection accepted")
	}
}
func TestUnknownSchemaReadOnly(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(p.ConfigFile), 0700); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"schema_version":2,"future_field":{"keep":true}}`)
	if err := os.WriteFile(p.ConfigFile, future, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithPaths(p)
	if !errors.Is(err, ErrUnknownSchema) || c.SchemaVersion != 2 {
		t.Fatalf("future config: %v %+v", err, c)
	}
	if err := SaveWithPaths(p, testConfig(p)); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("future overwrite not refused: %v", err)
	}
	got, _ := os.ReadFile(p.ConfigFile)
	if !bytes.Equal(got, future) {
		t.Fatal("future config overwritten")
	}
}
func TestConfigHasNoCredentials(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var checkFields func(any)
	checkFields = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				for _, forbidden := range []string{"secret", "access_token", "refresh_token", "device_code", "credential"} {
					if strings.Contains(strings.ToLower(key), forbidden) {
						t.Fatalf("credential field %s serialized", key)
					}
				}
				checkFields(child)
			}
		case []any:
			for _, child := range v {
				checkFields(child)
			}
		}
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	checkFields(document)
	loaded, err := LoadWithPaths(p)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Mounts[0].WriteBack != "2s" || loaded.Profiles[0].ID != profileID {
		t.Fatalf("bad round trip: %+v", loaded)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p.ConfigFile)
		if info.Mode().Perm() != 0600 {
			t.Fatalf("config mode %o", info.Mode().Perm())
		}
		info, _ = os.Stat(filepath.Dir(p.ConfigFile))
		if info.Mode().Perm() != 0700 {
			t.Fatalf("directory mode %o", info.Mode().Perm())
		}
	}
}
func TestValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"duplicate UUID", func(c *Config) { c.Remotes[0].ID = profileID }},
		{"invalid UUID", func(c *Config) { c.Profiles[0].ID = "work" }},
		{"missing profile", func(c *Config) { c.Remotes[0].ProfileID = mountID }},
		{"missing remote", func(c *Config) { c.Mounts[0].RemoteID = profileID }},
		{"relative mount", func(c *Config) { c.Mounts[0].Mountpoint = "relative" }},
		{"root mount", func(c *Config) { c.Mounts[0].Mountpoint = string(filepath.Separator) }},
		{"negative capacity", func(c *Config) { c.Mounts[0].CacheMaxBytes = -1 }},
		{"duration", func(c *Config) { c.Mounts[0].WriteBack = "invalid" }},
		{"doc limit", func(c *Config) { c.Mounts[0].MaxDocBytes = 10485761 }},
		{"auth path", func(c *Config) { c.Profiles[0].AuthRef = "../other"; c.Profiles[0].IdentityHash = "hash" }},
		{"partial identity", func(c *Config) { c.Profiles[0].IdentityHash = "hash" }},
		{"recursive state", func(c *Config) { c.StateDir = filepath.Join(c.Mounts[0].Mountpoint, "state") }},
		{"recursive cache", func(c *Config) { c.CacheDir = filepath.Dir(c.Mounts[0].Mountpoint) }},
		{"relative override", func(c *Config) { c.StateDir = "state" }},
		{"cache mode", func(c *Config) { c.Mounts[0].CacheMode = "off" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPaths(t)
			c := testConfig(p)
			tt.mutate(&c)
			if err := Validate(c, p); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	p := testPaths(t)
	if err := Validate(testConfig(p), p); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
func TestConcurrentSaveAtomicJSON(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	errs := make(chan error, 16)
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			got, err := LoadWithPaths(p)
			if err != nil {
				errs <- err
				return
			}
			if !reflect.DeepEqual(got, c) {
				errs <- errors.New("reader observed an incomplete configuration")
				return
			}
		}
	})
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Go(func() {
			if err := SaveWithPaths(p, c); err != nil {
				errs <- err
			}
		})
	}
	writers.Wait()
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSavePreservesOpenConfigReader(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	// lib/file opens with FILE_SHARE_DELETE on Windows, allowing a reader to
	// retain its complete old snapshot while the pathname is replaced.
	reader, err := file.Open(p.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	c.Profiles[0].Name = "replacement"
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var old Config
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, testConfig(p)) {
		t.Fatalf("existing reader lost its original snapshot: %+v", old)
	}
	got, err := LoadWithPaths(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("new reader did not see the complete replacement: %+v", got)
	}
}
func TestIdentityRegistryCanonicalRoots(t *testing.T) {
	p := testPaths(t)
	ref := "auth/identities/" + identityID + "/lark"
	r := model.IdentityRegistry{SchemaVersion: 1, Identities: []model.IdentityRecord{{ID: identityID, VerifiedIdentity: model.VerifiedIdentity{Brand: "feishu", AppID: "app", UserOpenID: "user", IdentityHash: "hash"}, AuthRef: ref}}}
	profile := Profile{ID: profileID, Brand: "feishu", AuthRef: ref, IdentityHash: "hash"}
	got, err := ResolveAuthRoot(p, r, profile)
	if err != nil || got != filepath.Join(p.StateDir, filepath.FromSlash(ref)) {
		t.Fatalf("resolve identity: %q %v", got, err)
	}
	profile.IdentityHash = "other"
	if _, err := ResolveAuthRoot(p, r, profile); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("identity mismatch: %v", err)
	}
	duplicate := r.Identities[0]
	duplicate.ID = "55555555-5555-4555-8555-555555555555"
	duplicate.AuthRef = "auth/identities/" + duplicate.ID + "/lark"
	r.Identities = append(r.Identities, duplicate)
	if err := ValidateIdentityRegistry(r); err == nil {
		t.Fatal("one identity bound to multiple roots")
	}
	r.Identities[1].UserOpenID = "different"
	r.Identities[1].AuthRef = ref
	if err := ValidateIdentityRegistry(r); err == nil {
		t.Fatal("different identities sharing root")
	}
}
func TestDefaultsHaveNoExperimentState(t *testing.T) {
	c := Default()
	if c.SchemaVersion != 1 || len(c.Profiles) != 0 || len(c.Remotes) != 0 || len(c.Mounts) != 0 || c.StateDir != "" || c.CacheDir != "" {
		t.Fatalf("default contains state: %+v", c)
	}
	p, err := DefaultPaths()
	if err != nil || !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.ConfigFile) || !filepath.IsAbs(p.CacheDir) {
		t.Fatalf("default paths: %+v %v", p, err)
	}
}

func TestSaveClaimsPrivateRoots(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{p.StateDir, p.CacheDir} {
		info, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
			t.Fatalf("private root mode %o", info.Mode().Perm())
		}
	}
	other := p
	other.ConfigFile = filepath.Join(filepath.Dir(p.ConfigFile), "other.json")
	if err := SaveWithPaths(other, c); err == nil {
		t.Fatal("different config acquired same private roots")
	}
}
func TestSaveRejectsOccupiedOverrides(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	c.StateDir = filepath.Join(filepath.Dir(p.StateDir), "occupied")
	if err := os.Mkdir(c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.StateDir, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveWithPaths(p, c); err == nil {
		t.Fatal("occupied override accepted")
	}
}
func TestDefaultMountParameters(t *testing.T) {
	m := DefaultMount()
	if m.CacheMode != "full" || m.CacheMaxBytes != 5368709120 || m.WriteBack != "2s" || m.PollDocs != "5s" || m.RefreshDirs != "30s" || m.MaxDocBytes != 10485760 || !m.Notify || m.Autostart || m.MountEngine != "auto" {
		t.Fatalf("unexpected defaults %+v", m)
	}
}

func TestRejectOccupiedOverrideWithoutChangingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode assertion")
	}
	p := testPaths(t)
	c := testConfig(p)
	c.StateDir = filepath.Join(filepath.Dir(p.StateDir), "occupied")
	if err := os.Mkdir(c.StateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.StateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.StateDir, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveWithPaths(p, c); err == nil {
		t.Fatal("occupied override accepted")
	}
	info, err := os.Stat(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatal("rejected override permissions changed")
	}
}
func TestSymlinkCannotAliasStateIntoMount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native symlink privilege gate")
	}
	p := testPaths(t)
	c := testConfig(p)
	target := filepath.Join(filepath.Dir(p.StateDir), "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p.StateDir); err != nil {
		t.Fatal(err)
	}
	if err := Validate(c, p); err == nil {
		t.Fatal("symlink state accepted")
	}
}
func TestPublicLoadSaveAndMissing(t *testing.T) {
	p := testPaths(t)
	c, err := Load(p.ConfigFile)
	if err != nil || c.SchemaVersion != 1 {
		t.Fatalf("missing config: %+v %v", c, err)
	}
	// Overrides keep the public path-based API entirely inside the temporary user root.
	c = testConfig(p)
	c.StateDir = p.StateDir
	c.CacheDir = p.CacheDir
	if err := Save(p.ConfigFile, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p.ConfigFile)
	if err != nil || got.Mounts[0].ID != mountID {
		t.Fatalf("public roundtrip: %+v %v", got, err)
	}
}

func TestSaveChecksSchemaUnderWriterLock(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(p.ConfigFile + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	finished := make(chan error, 1)
	go func() { finished <- SaveWithPaths(p, c) }()
	select {
	case err := <-finished:
		t.Fatalf("save bypassed held writer lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	future := []byte(`{"schema_version":2,"future_data":"preserve"}`)
	if err := os.WriteFile(p.ConfigFile, future, 0600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrUnknownSchema) {
			t.Fatalf("future schema not rechecked: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("save did not resume")
	}
	got, err := os.ReadFile(p.ConfigFile)
	if err != nil || !bytes.Equal(got, future) {
		t.Fatalf("future config changed: %s %v", got, err)
	}
}

func isolatedDefaultPaths(t *testing.T) Paths {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for key, dir := range map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config-home", "XDG_CACHE_HOME": "cache-home", "XDG_STATE_HOME": "state-home", "APPDATA": "roaming", "LOCALAPPDATA": "local"} {
		t.Setenv(key, filepath.Join(root, dir))
	}
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	// Known Folder APIs intentionally ignore LOCALAPPDATA overrides on Windows.
	// Use an injected config/state pair so this filesystem test touches only TempDir.
	p.ConfigFile = filepath.Join(root, "config", "config.json")
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	p.StateDir = filepath.Join(cacheBase, "DriveBridge", "State")
	return p
}

func TestDefaultCacheDedicatedRoot(t *testing.T) {
	p := isolatedDefaultPaths(t)
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cacheBase, "DriveBridge", "Cache")
	if p.CacheDir != want {
		t.Fatalf("default cache root = %q, want dedicated evictable root %q", p.CacheDir, want)
	}
}

func TestDefaultCacheSharesBrandParentWithState(t *testing.T) {
	p := isolatedDefaultPaths(t)
	// This reproduces the Windows State/cache-base relationship on every host.
	brandRoot := filepath.Dir(p.StateDir)
	tools := filepath.Join(brandRoot, "validation-tools")
	if err := os.MkdirAll(tools, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(tools, "component.txt")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveWithPaths(p, Default()); err != nil {
		t.Fatalf("default cache cannot coexist with sibling State/component directories: %v", err)
	}
	if _, err := LoadWithPaths(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(brandRoot, ".drivebridge-owner")); !os.IsNotExist(err) {
		t.Fatalf("brand parent was claimed: %v", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("other brand component changed: %q %v", data, err)
	}
}

func TestMountMacOSBackendValidationAndPersistence(t *testing.T) {
	for _, backend := range []string{"", "kernel", "fskit", "auto", "unknown"} {
		t.Run(backend, func(t *testing.T) {
			p := testPaths(t)
			c := testConfig(p)
			c.Mounts[0].MacOSBackend = backend
			valid := backend == "" || runtime.GOOS == "darwin" && (backend == "kernel" || backend == "fskit")
			err := SaveWithPaths(p, c)
			if !valid {
				if err == nil {
					t.Fatalf("accepted backend %q on %s", backend, runtime.GOOS)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := LoadWithPaths(p)
			if err != nil || saved.Mounts[0].MacOSBackend != backend {
				t.Fatalf("backend not preserved: %+v %v", saved.Mounts, err)
			}
			raw, err := os.ReadFile(p.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			if backend == "" && bytes.Contains(raw, []byte("macos_backend")) {
				t.Fatal("default config gained a backend override")
			}
		})
	}
}

func TestCacheMaxAgeDefaultsLegacyAndCapacityOnly(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{{"", 7 * 24 * time.Hour}, {"168h", 7 * 24 * time.Hour}, {"720h", 30 * 24 * time.Hour}, {"0h", 0}} {
		p := testPaths(t)
		cfg := testConfig(p)
		cfg.Mounts[0].CacheMaxAge = test.value
		if err := Validate(cfg, p); err != nil {
			t.Fatal(test.value, err)
		}
		age, err := EffectiveCacheMaxAge(cfg.Mounts[0])
		if err != nil || age != test.want {
			t.Fatal(test.value, age, err)
		}
		if err := SaveWithPaths(p, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadWithPaths(p)
		if err != nil || loaded.Mounts[0].CacheMaxAge != test.value {
			t.Fatal(loaded, err)
		}
		if age, err := EffectiveCacheMaxAge(loaded.Mounts[0]); err != nil || age != test.want {
			t.Fatal(age, err)
		}
	}
	if DefaultMount().CacheMaxAge != "168h" {
		t.Fatal(DefaultMount())
	}
	for _, invalid := range []string{"-1h", "7d", "invalid", "999999999999999999h"} {
		p := testPaths(t)
		cfg := testConfig(p)
		cfg.Mounts[0].CacheMaxAge = invalid
		if err := Validate(cfg, p); err == nil {
			t.Fatal("invalid age accepted", invalid)
		}
	}
}
