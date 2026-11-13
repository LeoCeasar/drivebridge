package larkcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Drive struct {
	runner  *Runner
	tempDir string
}

func NewDrive(r *Runner, tempDir string) *Drive { return &Drive{runner: r, tempDir: tempDir} }

type DriveItem struct {
	Token        string `json:"token"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	ParentToken  string `json:"parent_token,omitempty"`
	URL          string `json:"url,omitempty"`
	CreatedTime  string `json:"created_time,omitempty"`
	ModifiedTime string `json:"modified_time,omitempty"`
}
type DrivePage struct {
	Items      []DriveItem
	HasMore    bool
	NextCursor string
}

func decodePage(data []byte, parent string) (DrivePage, error) {
	var raw struct {
		Files   *[]json.RawMessage `json:"files"`
		HasMore *bool              `json:"has_more"`
		Cursor  *string            `json:"next_page_token"`
	}
	var out DrivePage
	if singleJSON(data, &raw) != nil || raw.Files == nil || raw.HasMore == nil {
		return out, ErrProtocol
	}
	out.HasMore = *raw.HasMore
	if raw.Cursor != nil {
		out.NextCursor = *raw.Cursor
	}
	if (out.HasMore && out.NextCursor == "") || (!out.HasMore && out.NextCursor != "") {
		return out, ErrProtocol
	}
	for _, r := range *raw.Files {
		var item struct {
			Token, Name, Type *string
			ParentToken       string `json:"parent_token"`
			URL               string
			CreatedTime       string `json:"created_time"`
			ModifiedTime      string `json:"modified_time"`
		}
		if singleJSON(r, &item) != nil || item.Token == nil || *item.Token == "" || item.Name == nil || *item.Name == "" || item.Type == nil || *item.Type == "" {
			return DrivePage{}, ErrProtocol
		}
		if parent != "" && item.ParentToken != "" && item.ParentToken != parent {
			return DrivePage{}, ErrProtocol
		}
		for _, v := range []string{item.CreatedTime, item.ModifiedTime} {
			if v != "" {
				if _, err := strictInt([]byte(v), false); err != nil {
					return DrivePage{}, ErrProtocol
				}
			}
		}
		out.Items = append(out.Items, DriveItem{Token: *item.Token, Name: *item.Name, Type: *item.Type, ParentToken: item.ParentToken, URL: item.URL, CreatedTime: item.CreatedTime, ModifiedTime: item.ModifiedTime})
	}
	return out, nil
}

// List returns only a complete scan. Failure returns nil, leaving any caller's
// previous complete snapshot untouched; absence never establishes deletion.
func (d *Drive) List(ctx context.Context, folder string) ([]DriveItem, error) {
	var items []DriveItem
	seen := map[string]bool{"": true}
	byToken := map[string]DriveItem{}
	cursor := ""
	for page := 0; page < 10000; page++ {
		args := []string{"drive", "files", "list"}
		if folder != "" {
			args = append(args, "--folder-token", folder, "--page-size", "200")
			if cursor != "" {
				args = append(args, "--page-token", cursor)
			}
		}
		resp, err := d.runner.Run(ctx, Request{Args: args})
		if err != nil {
			return nil, err
		}
		p, err := decodePage(resp.Data, folder)
		if err != nil {
			return nil, err
		}
		for _, item := range p.Items {
			if old, ok := byToken[item.Token]; ok {
				if !reflect.DeepEqual(old, item) {
					return nil, ErrProtocol
				}
				continue
			}
			byToken[item.Token] = item
			items = append(items, item)
		}
		if !p.HasMore {
			return items, nil
		}
		if folder == "" || seen[p.NextCursor] {
			return nil, ErrProtocol
		}
		seen[p.NextCursor] = true
		cursor = p.NextCursor
	}
	return nil, ErrProtocol
}

type HistoryItem struct {
	Version        string
	Name           *string
	Size, EditTime int64
	IsDeleted      bool
	Type, Tag      *int64
}
type HistoryPage struct {
	Items   []HistoryItem
	HasMore bool
}

var integerRE = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var versionRE = regexp.MustCompile(`^[0-9]{1,19}$`)

func strictInt(raw []byte, quoted bool) (int64, error) {
	s := string(raw)
	if quoted && strings.HasPrefix(s, `"`) {
		if json.Unmarshal(raw, &s) != nil {
			return 0, ErrProtocol
		}
	}
	if !integerRE.MatchString(s) {
		return 0, ErrProtocol
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrProtocol
	}
	return v, nil
}
func DecodeHistory(data []byte) (HistoryPage, error) {
	var raw struct {
		Items   *[]json.RawMessage `json:"items"`
		HasMore *bool              `json:"has_more"`
	}
	var out HistoryPage
	if singleJSON(data, &raw) != nil || raw.Items == nil || raw.HasMore == nil {
		return out, ErrProtocol
	}
	out.HasMore = *raw.HasMore
	for _, r := range *raw.Items {
		var x struct {
			Version   string          `json:"version"`
			Name      *string         `json:"name"`
			Size      json.RawMessage `json:"size"`
			EditTime  json.RawMessage `json:"edit_time"`
			Deleted   *bool           `json:"is_deleted"`
			Type, Tag json.RawMessage
		}
		if singleJSON(r, &x) != nil || !versionRE.MatchString(x.Version) || x.Deleted == nil {
			return HistoryPage{}, ErrProtocol
		}
		size, err := strictInt(x.Size, true)
		if err != nil {
			return HistoryPage{}, err
		}
		edit, err := strictInt(x.EditTime, false)
		if err != nil {
			return HistoryPage{}, err
		}
		item := HistoryItem{Version: x.Version, Name: x.Name, Size: size, EditTime: edit, IsDeleted: *x.Deleted}
		for i, v := range []json.RawMessage{x.Type, x.Tag} {
			if len(v) > 0 {
				n, err := strictInt(v, false)
				if err != nil {
					return HistoryPage{}, err
				}
				if i == 0 {
					item.Type = &n
				} else {
					item.Tag = &n
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}
func (d *Drive) HistoryPage(ctx context.Context, token, lastEditTime string) (HistoryPage, error) {
	if token == "" {
		return HistoryPage{}, ErrProtocol
	}
	params := struct {
		OnlyTag      bool   `json:"only_tag"`
		PageSize     int    `json:"page_size"`
		LastEditTime string `json:"last_edit_time,omitempty"`
	}{false, 200, lastEditTime}
	if lastEditTime != "" {
		if _, err := strictInt([]byte(lastEditTime), false); err != nil {
			return HistoryPage{}, err
		}
	}
	p, _ := json.Marshal(params)
	r, err := d.runner.Run(ctx, Request{Args: []string{"api", "GET", "/open-apis/drive/v1/files/" + url.PathEscape(token) + "/history", "--params", string(p)}})
	if err != nil {
		return HistoryPage{}, err
	}
	return DecodeHistory(r.Data)
}

type FileMeta struct {
	Token, Version string
	Size           int64
	ModTime        time.Time
}

// No verified current-version source exists in the pinned evidence. History's
// order, requested versions and upload-result versions cannot establish head.
func (d *Drive) FileMeta(ctx context.Context, token string) (FileMeta, error) {
	return FileMeta{}, ErrMetadataUnknown
}

// UploadResult may contain unconfirmed candidate metadata with an error. Callers
// must retain the error and independently reconcile the candidate before adoption.
type UploadResult struct {
	FileToken    string
	Size         int64
	URL, Version string
}

func (d *Drive) Upload(ctx context.Context, parent, token, name string, in io.Reader) (UploadResult, error) {
	dir, err := os.MkdirTemp(d.tempDir, "drivebridge-upload-")
	if err != nil {
		return UploadResult{}, errors.Join(ErrNotDispatched, err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "input")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return UploadResult{}, errors.Join(ErrNotDispatched, err)
	}
	size, copyErr := io.Copy(f, in)
	closeErr := f.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return UploadResult{}, errors.Join(ErrNotDispatched, err)
	}
	args := []string{"drive", "+upload", "--file", "input", "--name", name}
	if token != "" {
		args = append(args, "--file-token", token)
	} else if parent != "" {
		args = append(args, "--folder-token", parent)
	}
	r, runErr := d.runner.Run(ctx, Request{Args: args, Dir: dir, Timeout: transferTimeout})
	if errors.Is(runErr, ErrNotDispatched) {
		return UploadResult{}, runErr
	}
	result, metadataErr := uploadEvidence(r.Data)
	if result.Size != size || (token != "" && result.FileToken != token) {
		metadataErr = ErrProtocol
	}
	if err := errors.Join(runErr, metadataErr); err != nil {
		return result, errors.Join(ErrUnknownResult, err)
	}
	return result, nil
}

// Decode fields independently so one malformed field cannot erase other valid
// evidence. Only a complete JSON value can supply candidate metadata.
func uploadEvidence(data []byte) (UploadResult, error) {
	var raw struct {
		Token   json.RawMessage `json:"file_token"`
		Size    json.RawMessage `json:"size"`
		URL     json.RawMessage `json:"url"`
		Version json.RawMessage `json:"version"`
	}
	if singleJSON(data, &raw) != nil {
		return UploadResult{}, ErrProtocol
	}
	var result UploadResult
	invalid := json.Unmarshal(raw.Token, &result.FileToken) != nil || result.FileToken == ""
	var err error
	result.Size, err = strictInt(raw.Size, false)
	if err != nil {
		invalid = true
	}
	if len(raw.URL) > 0 && json.Unmarshal(raw.URL, &result.URL) != nil {
		invalid = true
	}
	if len(raw.Version) > 0 && json.Unmarshal(raw.Version, &result.Version) != nil {
		invalid = true
	}
	if invalid {
		return result, ErrProtocol
	}
	return result, nil
}
func (d *Drive) mutation(ctx context.Context, args []string) (Response, error) {
	r, err := d.runner.Run(ctx, Request{Args: args})
	if err != nil {
		return r, mutationError(err)
	}
	return r, nil
}
func (d *Drive) CreateFolder(ctx context.Context, parent, name string) (string, error) {
	args := []string{"drive", "+create-folder", "--name", name}
	if parent != "" {
		args = append(args, "--folder-token", parent)
	}
	r, err := d.mutation(ctx, args)
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"folder_token"`
	}
	if singleJSON(r.Data, &out) != nil || out.Token == "" {
		return "", errors.Join(ErrUnknownResult, ErrProtocol)
	}
	return out.Token, nil
}
func (d *Drive) Delete(ctx context.Context, token, kind string) error {
	_, err := d.mutation(ctx, []string{"drive", "+delete", "--file-token", token, "--type", kind, "--yes"})
	return err
}
func (d *Drive) Move(ctx context.Context, token, kind, parent string) error {
	args := []string{"drive", "+move", "--file-token", token, "--type", kind}
	if parent != "" {
		args = append(args, "--folder-token", parent)
	}
	_, err := d.mutation(ctx, args)
	return err
}
func (d *Drive) Rename(ctx context.Context, token, kind, name string) error {
	body, _ := json.Marshal(struct {
		Title string `json:"new_title"`
	}{name})
	_, err := d.runner.Run(ctx, Request{Args: []string{"drive", "files", "patch", "--file-token", token, "--type", kind, "--data", "-"}, Stdin: body})
	if err != nil {
		return mutationError(err)
	}
	return nil
}
