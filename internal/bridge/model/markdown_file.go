package model

import (
	"path"
	"strings"
)

// IsMarkdownFile selects ordinary cloud Markdown files, not the Markdown
// projections of native documents. CloudName survives local name mapping.
func IsMarkdownFile(entry Entry) bool {
	name := entry.CloudName
	if name == "" {
		name = entry.Remote
	}
	return entry.Kind == KindFile && strings.EqualFold(path.Ext(name), ".md")
}

func (s Snapshot) ContentBlob() BlobID {
	if s.FileData != "" {
		return s.FileData
	}
	return s.Markdown
}

// ConflictVersion identifies captured content for a human resolution. A file's
// SHA-256 is a local observation key, never a server document revision or CAS.
func (s Snapshot) ConflictVersion() string {
	if s.FileData != "" {
		return "sha256:" + string(s.FileData)
	}
	return s.Revision
}
