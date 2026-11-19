package larkcli

import (
	"context"
	"strings"
	"testing"
)

func TestResourceURLUsesTypedProviderMetadata(t *testing.T) {
	d := NewDrive(NewDirect(testExecutable(t, "link-meta")), "")
	got, err := d.ResourceURL(context.Background(), "doc-token", "docx")
	if err != nil || got != "https://tenant.feishu.cn/docx/doc-token" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestResourceURLRejectsWrongIdentityAndUnsafeURLs(t *testing.T) {
	for _, scenario := range []string{"link-wrong", "link-missing", "link-unsafe", "link-failed"} {
		t.Run(scenario, func(t *testing.T) {
			got, err := NewDrive(NewDirect(testExecutable(t, scenario)), "").ResourceURL(context.Background(), "doc-token", "docx")
			if err == nil || got != "" {
				t.Fatalf("%q %v", got, err)
			}
		})
	}
	for _, u := range []string{"http://tenant.feishu.cn/docx/a", "https://user:password@host/a", "https://host/a\nURL=bad", "https://host/%0a", "https://host/a\\b", "javascript:bad"} {
		if SafeResourceURL(u) != "" {
			t.Fatal("accepted", u)
		}
	}
}

func handleLinkProcess(s string, args []string, emit func(string)) bool {
	if !strings.HasPrefix(s, "link-") {
		return false
	}
	if len(args) < 3 || strings.Join(args[:3], " ") != "api POST /open-apis/drive/v1/metas/batch_query" || arg(args, "--data") == "" {
		return true
	}
	data := arg(args, "--data")
	if !strings.Contains(data, `"with_url":true`) || !strings.Contains(data, `"doc_token":"doc-token"`) || !strings.Contains(data, `"doc_type":"docx"`) {
		return true
	}
	switch s {
	case "link-meta":
		emit(`{"metas":[{"doc_token":"doc-token","doc_type":"docx","url":"https://tenant.feishu.cn/docx/doc-token"}]}`)
	case "link-wrong":
		emit(`{"metas":[{"doc_token":"other","doc_type":"docx","url":"https://tenant.feishu.cn/docx/other"}]}`)
	case "link-missing":
		emit(`{"metas":[]}`)
	case "link-unsafe":
		emit(`{"metas":[{"doc_token":"doc-token","doc_type":"docx","url":"https://host/%0a"}]}`)
	case "link-failed":
		emit(`{"metas":[],"failed_list":[{"token":"doc-token","code":403}]}`)
	}
	return true
}
