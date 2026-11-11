package ownedlogin

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/discovery"
)

func canonicalID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}
func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || strings.Contains(u.Host, ":") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", ErrProtocol
	}
	host := strings.ToLower(u.Hostname())
	tenant := ""
	for _, suffix := range []string{".feishu.cn", ".larksuite.com"} {
		if strings.HasSuffix(host, suffix) {
			tenant = strings.TrimSuffix(host, suffix)
		}
	}
	if tenant == "" || len(host) > 253 {
		return "", ErrProtocol
	}
	for _, label := range strings.Split(tenant, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrProtocol
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", ErrProtocol
			}
		}
	}
	return "https://" + host, nil
}
func validRequest(path string, q url.Values) bool {
	switch path {
	case "/space/api/explorer/v3/my_space/folder/", "/space/api/explorer/v3/my_space/obj/", "/space/api/explorer/v2/share/folder/list/":
	default:
		return false
	}
	if len(q.Encode()) > 8192 {
		return false
	}
	for key, values := range q {
		switch key {
		case "length", "asc", "rank", "hidden", "last_label":
		default:
			return false
		}
		if len(values) != 1 || len(values[0]) > 4096 || !utf8.ValidString(values[0]) {
			return false
		}
		for _, r := range values[0] {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

type statusError struct{ status int }

func (e statusError) Error() string   { return "native directory request failed" }
func (e statusError) StatusCode() int { return e.status }
func validateResult(result *helperResult) ([]byte, error) {
	if result == nil {
		return nil, ErrProtocol
	}
	if result.Kind == "login" || result.Kind == "needs_login" || result.Status == 401 || result.Status == 403 {
		return nil, discovery.ErrNeedsLogin
	}
	if result.Status == 429 || (result.Status >= 500 && result.Status <= 599) {
		return nil, statusError{result.Status}
	}
	if result.Kind != "" || result.Status < 200 || result.Status >= 300 || len(result.Body) > maxBody || !utf8.ValidString(result.Body) {
		return nil, ErrProtocol
	}
	if strings.Contains(strings.ToLower(result.ContentType), "text/html") {
		return nil, discovery.ErrNeedsLogin
	}
	if !strings.Contains(strings.ToLower(result.ContentType), "application/json") {
		return nil, ErrProtocol
	}
	var envelope map[string]json.RawMessage
	var code *int
	if json.Unmarshal([]byte(result.Body), &envelope) != nil || json.Unmarshal(envelope["code"], &code) != nil || code == nil {
		return nil, ErrProtocol
	}
	if *code == 399 {
		return nil, discovery.ErrNeedsLogin
	}
	if *code != 0 {
		return nil, ErrProtocol
	}
	return []byte(result.Body), nil
}
