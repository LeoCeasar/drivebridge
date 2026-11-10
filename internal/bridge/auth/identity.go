package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

var (
	ErrInvalid        = errors.New("invalid authentication configuration")
	ErrBusy           = errors.New("authentication operation already active")
	ErrNeedsReconcile = errors.New("authentication result requires reconciliation")
	ErrUserMissing    = errors.New("application configured; user authorization required")
	ErrMissingScopes  = errors.New("required authorization scopes missing")
	ErrReferencesBusy = errors.New("affected credential references are active or unverified")
	ErrFlowExpired    = errors.New("authorization flow expired")
	ErrFlowNotFound   = errors.New("authorization flow not found")
)

type Identity struct {
	Brand      string   `json:"brand"`
	AppID      string   `json:"app_id"`
	UserOpenID string   `json:"user_open_id"`
	Verified   bool     `json:"verified"`
	Scopes     []string `json:"scopes"`
}
type Binding struct {
	Identity
	AuthRef      string `json:"auth_ref"`
	IdentityHash string `json:"identity_hash"`
	AuthRoot     string `json:"-"`
}
type Reference struct {
	ConfigID     string          `json:"config_id"`
	ConfigFile   string          `json:"config_file"`
	ProfileID    model.ProfileID `json:"profile_id"`
	AuthRef      string          `json:"auth_ref"`
	IdentityHash string          `json:"identity_hash"`
}

// LifecycleGuard must prove ALL references/related workers and pending jobs idle,
// and keep them frozen until its release callback. Unknown/stale workers are busy.
type LifecycleGuard interface {
	Acquire(context.Context, string, []Reference) (func(), error)
}
type GuardFunc func(context.Context, string, []Reference) (func(), error)

func (f GuardFunc) Acquire(ctx context.Context, app string, refs []Reference) (func(), error) {
	return f(ctx, app, refs)
}

// ProfileStore lets the manager serialize profile binding with its other config edits.
type ProfileStore interface {
	Profile(context.Context, model.ProfileID) (model.Profile, error)
	Bind(context.Context, model.ProfileID, string, string) error
}

// FileProfiles is suitable for a single config writer. Managers sharing other
// writers should provide their own serialized ProfileStore implementation.
type FileProfiles struct{ ConfigFile string }

func (f FileProfiles) Profile(ctx context.Context, id model.ProfileID) (model.Profile, error) {
	if err := ctx.Err(); err != nil {
		return model.Profile{}, err
	}
	c, err := config.Load(f.ConfigFile)
	if err != nil {
		return model.Profile{}, err
	}
	for _, p := range c.Profiles {
		if p.ID == id {
			return p, nil
		}
	}
	return model.Profile{}, os.ErrNotExist
}
func (f FileProfiles) Bind(ctx context.Context, id model.ProfileID, ref, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := config.Load(f.ConfigFile)
	if err != nil {
		return err
	}
	found := false
	for i := range c.Profiles {
		if c.Profiles[i].ID == id {
			c.Profiles[i].AuthRef = ref
			c.Profiles[i].IdentityHash = hash
			found = true
		}
	}
	if !found {
		return os.ErrNotExist
	}
	return config.Save(f.ConfigFile, c)
}
func IdentityHash(brand, app, user string) string {
	h := sha256.Sum256([]byte(brand + "\x00" + app + "\x00" + user))
	return hex.EncodeToString(h[:])
}
func scopes(raw string) []string { s := strings.Fields(raw); sort.Strings(s); return compact(s) }
func compact(s []string) []string {
	out := []string{}
	for _, v := range s {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}
func hasScopes(granted, required []string) bool {
	m := map[string]bool{}
	for _, s := range granted {
		m[s] = true
	}
	for _, s := range required {
		if !m[s] {
			return false
		}
	}
	return true
}

type enrollment struct {
	ID           string          `json:"id"`
	ConfigID     string          `json:"config_id"`
	ProfileID    model.ProfileID `json:"profile_id"`
	Brand        string          `json:"brand"`
	AuthRef      string          `json:"auth_ref"`
	AppID        string          `json:"app_id,omitempty"`
	ExpectedHash string          `json:"expected_hash,omitempty"`
	State        string          `json:"state"`
	CanonicalRef string          `json:"canonical_ref,omitempty"`
}
type registry struct {
	SchemaVersion int                    `json:"schema_version"`
	Identities    []model.IdentityRecord `json:"identities"`
	References    []Reference            `json:"references"`
	Enrollments   []enrollment           `json:"enrollments"`
	Disabled      []string               `json:"disabled_auth_refs,omitempty"`
}

func (s *Service) readRegistry() (registry, error) {
	var r registry
	path := filepath.Join(s.domain, "auth", "registry.json")
	if err := platform.CheckNoSymlinks(path); err != nil {
		return r, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return registry{SchemaVersion: 1}, nil
	}
	if err != nil {
		return r, err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return r, ErrInvalid
	}
	if err = validateRegistry(r); err != nil {
		return r, err
	}

	return r, nil
}
func (s *Service) saveRegistry(r registry) error {
	if err := validateRegistry(r); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(s.domain, "auth")
	if err = platform.EnsurePrivateDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".registry-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = platform.SecurePrivate(name, false); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = platform.ReplaceFile(name, filepath.Join(dir, "registry.json")); err != nil {
		return err
	}
	return platform.SyncDir(dir)
}
func validID(id string) bool {
	v, err := uuid.Parse(id)
	return err == nil && v != uuid.Nil && v.String() == strings.ToLower(id)
}
func validRef(ref string, enroll bool) bool {
	p := strings.Split(ref, "/")
	if len(p) != 4 || p[0] != "auth" || !validID(p[2]) || p[3] != "lark" {
		return false
	}
	return p[1] == "identities" || (enroll && p[1] == "enrollments")
}
func (s *Service) root(ref string) (string, error) {
	if !validRef(ref, true) {
		return "", ErrInvalid
	}
	p := filepath.Join(s.domain, filepath.FromSlash(ref))
	return p, platform.CheckNoSymlinks(p)
}
func (s *Service) findEnrollment(r registry, id model.ProfileID) (int, bool) {
	for i, e := range r.Enrollments {
		if e.ConfigID == s.configID && e.ProfileID == id {
			return i, true
		}
	}
	return 0, false
}
func (s *Service) references(r registry, app string) []Reference {
	refs := []Reference{}
	for _, ref := range r.References {
		for _, record := range r.Identities {
			if record.AppID == app && record.AuthRef == ref.AuthRef {
				refs = append(refs, ref)
				break
			}
		}
	}
	return refs
}
func (s *Service) guardApp(ctx context.Context, r registry, app string) (func(), error) {
	refs := s.references(r, app)
	if len(refs) == 0 {
		return func() {}, nil
	}
	if nilValue(s.guard) {
		return nil, ErrReferencesBusy
	}
	release, err := s.guard.Acquire(ctx, app, refs)
	if err != nil {
		return nil, ErrReferencesBusy
	}
	if release == nil {
		return nil, ErrReferencesBusy
	}
	return release, nil
}
func (s *Service) Resolve(ctx context.Context, id model.ProfileID) (Binding, error) {
	return s.resolve(ctx, id, false, false)
}
func (s *Service) resolve(ctx context.Context, id model.ProfileID, allowDisabled, allowPending bool) (Binding, error) {
	p, err := s.profiles.Profile(ctx, id)
	if err != nil {
		return Binding{}, err
	}
	r, err := s.readRegistry()
	if err != nil {
		return Binding{}, err
	}
	if !allowDisabled {
		for _, ref := range r.Disabled {
			if ref == p.AuthRef {
				return Binding{}, ErrUserMissing
			}
		}
	}
	for _, record := range r.Identities {
		if record.AuthRef == p.AuthRef && record.IdentityHash == p.IdentityHash && record.Brand == p.Brand {
			if !allowPending && pendingApp(r, record.AppID) {
				return Binding{}, ErrNeedsReconcile
			}
			found := false
			for _, ref := range r.References {
				if ref.ConfigID == s.configID && ref.ProfileID == id && ref.AuthRef == p.AuthRef && ref.IdentityHash == p.IdentityHash {
					found = true
				}
			}
			if !found {
				return Binding{}, model.ErrIdentityChanged
			}
			root, err := s.root(record.AuthRef)
			return Binding{Identity: Identity{Brand: record.Brand, AppID: record.AppID, UserOpenID: record.UserOpenID, Verified: true}, AuthRef: record.AuthRef, IdentityHash: record.IdentityHash, AuthRoot: root}, err
		}
	}
	return Binding{}, model.ErrIdentityChanged
}

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Chan, reflect.Map, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func validateRegistry(r registry) error {
	if err := config.ValidateIdentityRegistry(model.IdentityRegistry{SchemaVersion: r.SchemaVersion, Identities: r.Identities}); err != nil {
		return err
	}
	records := map[string]model.IdentityRecord{}
	for _, v := range r.Identities {
		if v.IdentityHash != IdentityHash(v.Brand, v.AppID, v.UserOpenID) {
			return ErrInvalid
		}
		records[v.AuthRef] = v
	}
	enrollmentIDs := map[string]bool{}
	profiles := map[string]bool{}
	for _, e := range r.Enrollments {
		profile := e.ConfigID + "/" + string(e.ProfileID)
		if !validID(e.ID) || !validID(string(e.ProfileID)) || e.ConfigID == "" || enrollmentIDs[e.ID] || profiles[profile] || !validRef(e.AuthRef, true) || (e.CanonicalRef != "" && !validRef(e.CanonicalRef, false)) || (e.Brand != "feishu" && e.Brand != "lark") || (e.State != "prepared" && e.State != "active" && e.State != "unknown" && e.State != "app_ready") || (e.State == "app_ready" && e.AppID == "") {
			return ErrInvalid
		}
		enrollmentIDs[e.ID] = true
		profiles[profile] = true
	}
	refs := map[string]bool{}
	for _, ref := range r.References {
		record, ok := records[ref.AuthRef]
		k := ref.ConfigID + "/" + string(ref.ProfileID)
		if !ok || record.IdentityHash != ref.IdentityHash || !validID(string(ref.ProfileID)) || !filepath.IsAbs(ref.ConfigFile) || key(filepath.Clean(ref.ConfigFile)) != ref.ConfigID || refs[k] {
			return ErrInvalid
		}
		refs[k] = true
	}
	disabled := map[string]bool{}
	for _, ref := range r.Disabled {
		if _, ok := records[ref]; !ok || disabled[ref] {
			return ErrInvalid
		}
		disabled[ref] = true
	}
	return nil
}

// References returns every config/profile that can be affected by this AppID's
// shared credentials. The guard must freeze all of them before examining jobs.
func (s *Service) References(ctx context.Context, id model.ProfileID) ([]Reference, error) {
	b, err := s.resolve(ctx, id, true, true)
	if err != nil {
		return nil, err
	}
	r, err := s.readRegistry()
	if err != nil {
		return nil, err
	}
	return s.references(r, b.AppID), nil
}

func pendingApp(r registry, app string) bool {
	for _, e := range r.Enrollments {
		if e.AppID == app {
			return true
		}
	}
	return false
}
