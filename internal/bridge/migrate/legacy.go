package migrate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
)

type LegacyFormat string

const (
	LegacyMacOS   LegacyFormat = "macos"
	LegacyWindows LegacyFormat = "windows"
)

type Legacy struct {
	Backend, Remote, RemotePath, Mountpoint string
	Autostart                               bool
	Ignored                                 []string
}
type LegacyRemote struct {
	Name, Type, FolderToken string
	Ignored                 []string
}

const maxLegacyBytes = 2 << 20

func limited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxLegacyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxLegacyBytes {
		return nil, errors.New("legacy input exceeds 2 MiB")
	}
	return b, nil
}

// ReadLegacy parses data only. It never sources a shell, expands variables or executes commands.
func ReadLegacy(r io.Reader, format LegacyFormat) (Legacy, error) {
	b, err := limited(r)
	if err != nil {
		return Legacy{}, err
	}
	values := map[string]string{}
	switch format {
	case LegacyWindows:
		var raw map[string]json.RawMessage
		d := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})))
		if err = d.Decode(&raw); err != nil {
			return Legacy{}, err
		}
		if d.Decode(new(any)) != io.EOF {
			return Legacy{}, errors.New("trailing settings data")
		}
		for k, v := range raw {
			key := strings.ToUpper(k)
			if key == "SCHEMAVERSION" || key == "SCHEMA_VERSION" {
				return Legacy{}, ErrUnknownSchema
			}
			var s string
			if err = json.Unmarshal(v, &s); err != nil {
				var boolean bool
				if json.Unmarshal(v, &boolean) != nil {
					return Legacy{}, errors.New("legacy setting must be scalar string or bool")
				}
				if boolean {
					s = "true"
				} else {
					s = "false"
				}
			}
			values[key] = s
		}
	case LegacyMacOS:
		scan := bufio.NewScanner(bytes.NewReader(b))
		scan.Buffer(make([]byte, 4096), maxLegacyBytes)
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok || !assignmentKey.MatchString(key) {
				return Legacy{}, errors.New("legacy settings contain executable syntax")
			}
			if key == "SCHEMA_VERSION" {
				return Legacy{}, ErrUnknownSchema
			}
			if _, exists := values[key]; exists {
				return Legacy{}, errors.New("duplicate legacy assignment")
			}
			value, err = parseLiteral(value)
			if err != nil {
				return Legacy{}, err
			}
			values[key] = value
		}
		if err = scan.Err(); err != nil {
			return Legacy{}, err
		}
	default:
		return Legacy{}, errors.New("unsupported legacy format")
	}
	result := Legacy{Backend: "feishu"}
	for k, v := range values {
		switch k {
		case "BACKEND":
			result.Backend = v
		case "REMOTE":
			result.Remote = v
		case "REMOTE_PATH", "REMOTEPATH":
			result.RemotePath = v
		case "MOUNT_POINT", "MOUNTPOINT":
			result.Mountpoint = v
		case "AUTO_START", "AUTOSTART":
			result.Autostart = strings.EqualFold(v, "true")
		default:
			result.Ignored = append(result.Ignored, k)
		}
	}
	if result.Remote == "" {
		return result, errors.New("legacy remote name missing")
	}
	return result, nil
}

var assignmentKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func parseLiteral(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if v[0] != '\'' {
		if strings.ContainsAny(v, " \t;|&`$()<>\\\"") {
			return "", errors.New("unquoted shell syntax is not importable")
		}
		return v, nil
	}
	var result strings.Builder
	for len(v) > 0 {
		if strings.HasPrefix(v, "\\'") {
			result.WriteByte('\'')
			v = v[2:]
			continue
		}
		if v[0] != '\'' {
			return "", errors.New("legacy value is not a literal")
		}
		v = v[1:]
		end := strings.IndexByte(v, '\'')
		if end < 0 {
			return "", errors.New("unterminated legacy quote")
		}
		result.WriteString(v[:end])
		v = v[end+1:]
	}
	return result.String(), nil
}
func ReadRcloneRemote(r io.Reader, name string) (LegacyRemote, error) {
	b, err := limited(r)
	if err != nil {
		return LegacyRemote{}, err
	}
	out := LegacyRemote{Name: name}
	section := ""
	found := false
	scan := bufio.NewScanner(bytes.NewReader(b))
	scan.Buffer(make([]byte, 4096), maxLegacyBytes)
	seen := map[string]bool{}
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == name {
				if found {
					return out, errors.New("duplicate rclone remote section")
				}
				found = true
			}
			continue
		}
		if section != name {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return out, errors.New("invalid rclone config assignment")
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if seen[k] {
			return out, errors.New("duplicate rclone option")
		}
		seen[k] = true
		switch k {
		case "type":
			out.Type = v
		case "root_folder_token":
			out.FolderToken = v
		default:
			out.Ignored = append(out.Ignored, k)
		}
	}
	if err = scan.Err(); err != nil {
		return out, err
	}
	if !found {
		return out, errors.New("rclone remote not found")
	}
	return out, nil
}

type ImportOptions struct {
	ProfileID                     model.ProfileID
	FolderToken, Mountpoint, Name string
}
type ImportPlan struct {
	BeforeSHA256             string
	After                    model.Config
	LegacyRemote, LegacyPath string
	Warnings                 []string
	Digest                   string
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func planDigest(p ImportPlan) string { p.Digest = ""; return digest(p) }
func (s *Service) PreviewImport(ctx context.Context, l Legacy, r LegacyRemote, o ImportOptions) (ImportPlan, error) {
	if err := ctx.Err(); err != nil {
		return ImportPlan{}, err
	}
	c, err := config.LoadWithPaths(s.paths)
	if err != nil {
		return ImportPlan{}, err
	}
	before, err := fileDigest(s.paths.ConfigFile)
	if err != nil {
		return ImportPlan{}, err
	}
	if l.Backend != "feishu" || r.Type != "feishu" || r.Name != l.Remote {
		return ImportPlan{}, errors.New("only selected feishu remote can be imported")
	}
	var profile model.Profile
	for _, p := range c.Profiles {
		if p.ID == o.ProfileID {
			profile = p
		}
	}
	if profile.ID == "" || profile.AuthRef == "" || profile.IdentityHash == "" {
		return ImportPlan{}, model.ErrIdentityChanged
	}
	token := o.FolderToken
	if token == "" {
		if strings.Trim(l.RemotePath, "/") != "" {
			return ImportPlan{}, errors.New("legacy path requires an explicitly resolved folder resource token")
		}
		token = r.FolderToken
	}
	if token == "" {
		return ImportPlan{}, errors.New("explicit folder resource token required")
	}
	name := o.Name
	if name == "" {
		name = l.Remote
	}
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return ImportPlan{}, errors.New("invalid import name")
	}
	for _, remote := range c.Remotes {
		if strings.EqualFold(remote.Name, name) {
			return ImportPlan{}, errors.New("import remote name already exists")
		}
	}
	mountpoint := o.Mountpoint
	if mountpoint == "" {
		mountpoint = l.Mountpoint
	}
	cloneBytes, _ := json.Marshal(c)
	var after model.Config
	_ = json.Unmarshal(cloneBytes, &after)
	remote := model.Remote{ID: model.RemoteID(uuid.NewString()), Name: name, ProfileID: profile.ID, FolderToken: token, NewMDPolicy: "docx"}
	mount := config.DefaultMount()
	mount.ID = model.MountID(uuid.NewString())
	mount.RemoteID = remote.ID
	mount.Name = name
	mount.Mountpoint = mountpoint
	mount.Autostart = false
	after.Remotes = append(after.Remotes, remote)
	after.Mounts = append(after.Mounts, mount)
	if err = config.Validate(after, s.paths); err != nil {
		return ImportPlan{}, err
	}
	plan := ImportPlan{BeforeSHA256: before, After: after, LegacyRemote: l.Remote, LegacyPath: l.RemotePath, Warnings: []string{"Legacy executable/RC/cache settings are not imported.", "Verified existing profile is required; legacy credentials and queues are not adopted.", "Legacy startup must be disabled through manager handoff before activating the imported mount."}}
	plan.Digest = planDigest(plan)
	return plan, nil
}

type ApplyResult struct {
	Backup                             Manifest
	ConfigChanged, NeedsStartupHandoff bool
}

func (s *Service) ApplyImport(ctx context.Context, p ImportPlan, approvalDigest, backupDir string) (result ApplyResult, err error) {
	if approvalDigest == "" || approvalDigest != p.Digest || planDigest(p) != p.Digest {
		return result, ErrPreviewChanged
	}
	err = s.quiesce(ctx, func(ctx context.Context) error {
		before, err := fileDigest(s.paths.ConfigFile)
		if err != nil {
			return err
		}
		if before != p.BeforeSHA256 {
			return ErrPreviewChanged
		}
		if err = config.Validate(p.After, s.paths); err != nil {
			return err
		}
		result.Backup, err = s.backup(ctx, backupDir)
		if err != nil {
			return err
		}
		err = config.SaveWithPaths(s.paths, p.After)
		if err == nil {
			result.ConfigChanged = true
		} else if current, loadErr := config.LoadWithPaths(s.paths); loadErr == nil && digest(current) == digest(p.After) {
			result.ConfigChanged = true
		}
		result.NeedsStartupHandoff = result.ConfigChanged
		return err
	})
	return
}
