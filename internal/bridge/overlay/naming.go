package overlay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var deviceName = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\.|$)`)

func foldName(s string) string { return cases.Fold().String(norm.NFC.String(s)) }
func encodeName(s string) string {
	s = norm.NFC.String(s)
	trail := len(strings.TrimRight(s, " ."))
	reserved := deviceName.MatchString(s)
	var b strings.Builder
	for i, r := range s {
		escape := r == '%' || strings.ContainsRune(`<>:"/\|?*`, r) || unicode.IsControl(r) || (i >= trail && (r == '.' || r == ' ')) || (reserved && i == 0)
		if escape {
			for _, v := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", v)
			}
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func suffixName(name, token string, n int) string {
	sum := sha256.Sum256([]byte(token))
	h := hex.EncodeToString(sum[:])
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "~" + h[:n] + ext
}
func chooseName(name, token string, used map[string]bool) (string, error) {
	if !used[foldName(name)] {
		return name, nil
	}
	for _, n := range []int{8, 16, 64} {
		x := suffixName(name, token, n)
		if !used[foldName(x)] {
			return x, nil
		}
	}
	return "", errors.New("stable name hash collision")
}
func validPath(remote string, root bool) error {
	if remote == "" && root {
		return nil
	}
	if remote == "" || remote == "." || !utf8.ValidString(remote) || strings.HasPrefix(remote, "/") || strings.ContainsAny(remote, "\\\x00") || path.Clean(remote) != remote {
		return errors.New("noncanonical remote path")
	}
	for _, v := range strings.Split(remote, "/") {
		if v == ".." || v == "." || v == "" {
			return errors.New("unsafe remote path")
		}
	}
	return nil
}
func parentPath(p string) string {
	q := path.Dir(p)
	if q == "." {
		return ""
	}
	return q
}
