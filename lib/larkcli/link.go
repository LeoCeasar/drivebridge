package larkcli

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"unicode"
)

// SafeResourceURL admits provider-supplied HTTPS links, never credentials or
// shortcut/control-character injection. It does not synthesize resource URLs.
func SafeResourceURL(raw string) string {
	if strings.ContainsRune(raw, '\\') || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.ContainsRune(decoded, '\\') || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return ""
	}
	return raw
}

// ResourceURL reads metadata for one existing object. The POST is a read-only
// provider query; it neither changes link permissions nor exports document bodies.
func (d *Drive) ResourceURL(ctx context.Context, token, kind string) (string, error) {
	if token == "" || kind == "" {
		return "", ErrProtocol
	}
	body, err := json.Marshal(struct {
		Docs    []map[string]string `json:"request_docs"`
		WithURL bool                `json:"with_url"`
	}{[]map[string]string{{"doc_token": token, "doc_type": kind}}, true})
	if err != nil {
		return "", err
	}
	r, err := d.runner.Run(ctx, Request{Args: []string{"api", "POST", "/open-apis/drive/v1/metas/batch_query", "--data", string(body)}})
	if err != nil {
		return "", err
	}
	var out struct {
		Metas []struct {
			Token string `json:"doc_token"`
			Kind  string `json:"doc_type"`
			URL   string `json:"url"`
		} `json:"metas"`
		Failed []json.RawMessage `json:"failed_list"`
	}
	if singleJSON(r.Data, &out) != nil || len(out.Failed) != 0 || len(out.Metas) != 1 {
		return "", ErrMetadataUnknown
	}
	meta := out.Metas[0]
	if meta.Token != token || meta.Kind != kind {
		return "", ErrIdentity
	}
	if link := SafeResourceURL(meta.URL); link != "" {
		return link, nil
	}
	return "", ErrMetadataUnknown
}
