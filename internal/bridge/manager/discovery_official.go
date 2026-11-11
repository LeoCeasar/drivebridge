package manager

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

var discoveryTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
var discoveryUserRE = regexp.MustCompile(`^[1-9][0-9]{0,31}$`)

type discoveryRoot struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
}

func decodeDiscoveryRoot(data []byte) (discoveryRoot, error) {
	var root discoveryRoot
	if json.Unmarshal(data, &root) != nil || !discoveryTokenRE.MatchString(root.Token) || !discoveryUserRE.MatchString(root.UserID) {
		return root, discovery.ErrProtocol
	}
	return root, nil
}
func discoveryName(name string) bool {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func decodeDiscoveryFolder(data []byte, token string) (DiscoveryCandidate, error) {
	var result struct {
		Token, Name, ID string
		ParentID        string `json:"parentId"`
		Folder          *struct {
			Token, Name, ID string
			ParentID        string `json:"parentId"`
		}
	}
	if json.Unmarshal(data, &result) != nil {
		return DiscoveryCandidate{}, discovery.ErrProtocol
	}
	if result.Folder != nil {
		if result.Token != "" || result.Name != "" {
			return DiscoveryCandidate{}, discovery.ErrProtocol
		}
		result.Token, result.Name = result.Folder.Token, result.Folder.Name
		result.ID, result.ParentID = result.Folder.ID, result.Folder.ParentID
	}
	if result.Token != token || !discoveryTokenRE.MatchString(token) || !discoveryName(result.Name) {
		return DiscoveryCandidate{}, discovery.ErrProtocol
	}
	return DiscoveryCandidate{Token: token, Name: result.Name, nodeID: result.ID, parentID: result.ParentID}, nil
}
func discoveryOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || strings.Contains(u.Host, ":") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", discovery.ErrProtocol
	}
	host := strings.ToLower(u.Hostname())
	tenant := ""
	for _, suffix := range []string{".feishu.cn", ".larksuite.com"} {
		if strings.HasSuffix(host, suffix) {
			tenant = strings.TrimSuffix(host, suffix)
		}
	}
	if tenant == "" || len(host) > 253 {
		return "", discovery.ErrProtocol
	}
	for _, label := range strings.Split(tenant, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", discovery.ErrProtocol
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", discovery.ErrProtocol
			}
		}
	}
	return "https://" + host, nil
}
func discoveryFolderToken(input, origin string) (string, error) {
	input = strings.TrimSpace(input)
	if discoveryTokenRE.MatchString(input) {
		return input, nil
	}
	u, err := url.Parse(input)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawPath != "" || u.Fragment != "" {
		return "", discovery.ErrProtocol
	}
	candidate, err := discoveryOrigin(u.Scheme + "://" + u.Host)
	if err != nil || candidate != origin {
		return "", discovery.ErrIdentity
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) == 4 && parts[3] == "" {
		parts = parts[:3]
	}
	if len(parts) != 3 || parts[0] != "drive" || parts[1] != "folder" || !discoveryTokenRE.MatchString(parts[2]) {
		return "", discovery.ErrProtocol
	}
	return parts[2], nil
}
func (m *Manager) verifiedDiscovery(ctx context.Context, id model.ProfileID, origin string) (model.Profile, *larkcli.Runner, discoverySettings, error) {
	profile, err := m.ResolveProfile(ctx, string(id))
	if err != nil {
		return profile, nil, discoverySettings{}, err
	}
	a, err := m.Auth(ctx)
	if err != nil {
		return profile, nil, discoverySettings{}, err
	}
	runner, err := a.ManagedRunner(ctx, id)
	if err != nil {
		return profile, nil, discoverySettings{}, err
	}
	response, err := runner.Run(ctx, larkcli.Request{Args: []string{"api", "GET", "/open-apis/drive/explorer/v2/root_folder/meta"}})
	if err != nil {
		return profile, nil, discoverySettings{}, err
	}
	root, err := decodeDiscoveryRoot(response.Data)
	if err != nil {
		return profile, nil, discoverySettings{}, err
	}
	old, _, err := m.readDiscovery(ctx, id)
	if err != nil {
		return profile, nil, old, err
	}
	if origin == "" {
		origin = old.Binding.Origin
	}
	if origin == "" {
		items, e := larkcli.NewDrive(runner, "").List(ctx, "")
		if e != nil {
			return profile, nil, old, e
		}
		for _, item := range items {
			u, e := url.Parse(item.URL)
			if e != nil {
				continue
			}
			candidate, e := discoveryOrigin(u.Scheme + "://" + u.Host)
			if e == nil {
				if origin != "" && origin != candidate {
					return profile, nil, old, discovery.ErrIdentity
				}
				origin = candidate
			}
		}
	}
	origin, err = discoveryOrigin(origin)
	if err != nil {
		return profile, nil, old, err
	}
	binding := discovery.Binding{ProfileID: string(id), IdentityHash: profile.IdentityHash, UserID: root.UserID, Origin: origin}
	if old.Binding.ProfileID != "" && old.Binding != binding {
		return profile, nil, old, model.ErrIdentityChanged
	}
	old.Binding = binding
	old.RootToken = root.Token
	return profile, runner, old, nil
}
func discoveryFolder(ctx context.Context, runner *larkcli.Runner, token, origin string) (DiscoveryCandidate, error) {
	if !discoveryTokenRE.MatchString(token) {
		return DiscoveryCandidate{}, discovery.ErrProtocol
	}
	response, err := runner.Run(ctx, larkcli.Request{Args: []string{"api", "GET", "/open-apis/drive/explorer/v2/folder/" + token + "/meta"}})
	if err != nil {
		return DiscoveryCandidate{}, err
	}
	folder, err := decodeDiscoveryFolder(response.Data, token)
	if err == nil {
		folder.URL = origin + "/drive/folder/" + token
	}
	return folder, err
}

func decodeDiscoverySearch(data []byte, origin string) ([]string, bool, string, error) {
	var result struct {
		Results *[]struct {
			ResultMeta struct {
				Token, URL string
				DocTypes   json.RawMessage `json:"doc_types"`
			} `json:"result_meta"`
		} `json:"results"`
		HasMore   *bool  `json:"has_more"`
		PageToken string `json:"page_token"`
	}
	if json.Unmarshal(data, &result) != nil || result.Results == nil || result.HasMore == nil {
		return nil, false, "", discovery.ErrProtocol
	}
	out := make([]string, 0, len(*result.Results))
	for _, item := range *result.Results {
		var kinds []string
		var kind string
		if json.Unmarshal(item.ResultMeta.DocTypes, &kind) == nil {
			kinds = []string{kind}
		} else if json.Unmarshal(item.ResultMeta.DocTypes, &kinds) != nil {
			return nil, false, "", discovery.ErrProtocol
		}
		folder := false
		for _, kind := range kinds {
			if kind == "FOLDER" {
				folder = true
			}
		}
		if !folder {
			return nil, false, "", discovery.ErrProtocol
		}
		token := item.ResultMeta.Token
		if item.ResultMeta.URL != "" {
			fromURL, err := discoveryFolderToken(item.ResultMeta.URL, origin)
			if err != nil {
				return nil, false, "", err
			}
			if token != "" && token != fromURL {
				return nil, false, "", discovery.ErrProtocol
			}
			if token == "" {
				token = fromURL
			}
		}
		if !discoveryTokenRE.MatchString(token) {
			return nil, false, "", discovery.ErrProtocol
		}
		out = append(out, token)
	}
	if *result.HasMore && (len(out) == 0 || result.PageToken == "") {
		return nil, false, "", discovery.ErrProtocol
	}
	return out, *result.HasMore, result.PageToken, nil
}
