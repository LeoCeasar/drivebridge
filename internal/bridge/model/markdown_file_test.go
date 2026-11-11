package model

import "testing"

func TestMarkdownFilePolicyUsesCloudNameAndFinalSuffix(t *testing.T) {
	for _, tc := range []struct {
		entry Entry
		want  bool
	}{
		{Entry{Kind: KindFile, Remote: "目录/note.MD"}, true},
		{Entry{Kind: KindFile, Remote: "note.md", CloudName: "note.txt"}, false},
		{Entry{Kind: KindFile, Remote: "mapped.txt", CloudName: "cloud.Md"}, true},
		{Entry{Kind: KindFile, Remote: "folder.md/note.db"}, false},
		{Entry{Kind: KindFile, Remote: "note.md.bak"}, false},
		{Entry{Kind: KindDocx, Remote: "note.md"}, false},
		{Entry{Kind: KindFolder, Remote: "folder.md"}, false},
	} {
		if got := IsMarkdownFile(tc.entry); got != tc.want {
			t.Fatalf("%+v: got %t, want %t", tc.entry, got, tc.want)
		}
	}
}

func TestSnapshotConflictKeysSeparateFilesFromDocRevisions(t *testing.T) {
	file := Snapshot{FileData: "a-file-hash"}
	if file.ContentBlob() != file.FileData || file.ConflictVersion() != "sha256:a-file-hash" || file.Revision != "" {
		t.Fatal(file)
	}
	doc := Snapshot{Markdown: "markdown-hash", Revision: "42"}
	if doc.ContentBlob() != doc.Markdown || doc.ConflictVersion() != "42" {
		t.Fatal(doc)
	}
}
