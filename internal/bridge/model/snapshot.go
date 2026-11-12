package model

// Blobs enumerates every durable body owned by an immutable snapshot.
func (s Snapshot) Blobs() []BlobID {
	result := []BlobID{s.XML, s.Markdown, s.BlockMap, s.FileData}
	for _, b := range s.DocMedia {
		result = append(result, b)
	}
	return result
}
