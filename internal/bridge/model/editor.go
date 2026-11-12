package model

import "path"

// EditorTildeBackup accepts only a causal sibling of the original name, not an
// arbitrary rename into a similarly shaped user file.
func EditorTildeBackup(src, dst string) bool {
	if path.Dir(src) != path.Dir(dst) {
		return false
	}
	name := path.Base(src)
	ext := path.Ext(name)
	return path.Base(dst) == name+"~" || ext != "" && path.Base(dst) == name[:len(name)-len(ext)]+"~"+ext
}
