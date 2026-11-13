package markdown

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
)

// AssetPath uses only the opaque media token and a bounded image extension.
func AssetPath(token, name, mime string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp":
	default:
		switch strings.ToLower(mime) {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		case "image/avif":
			ext = ".avif"
		case "image/bmp":
			ext = ".bmp"
		default:
			ext = ".png"
		}
	}
	return ".drivebridge-assets/" + digest(token) + ext
}

func decodeReadable(b []byte, r string, o Options) (model.DocView, error) {
	if o.ProjectionVersion != 0 && o.ProjectionVersion != 2 && o.ProjectionVersion != 3 {
		return model.DocView{}, unsupported("unknown projection version")
	}
	d, err := decodeLegacy(b, r, o)
	if err != nil {
		return d, err
	}
	d.ProjectionVersion = 2
	if o.ProjectionVersion == 3 {
		d.ProjectionVersion = 3
	}
	var md []byte
	seen := map[string]bool{}
	var assets func(*xnode)
	assets = func(n *xnode) {
		if n.tag == "img" && n.attr("src") != "" {
			t := n.attr("src")
			assetPath := AssetPath(t, n.attr("name"), n.attr("mime"))
			if !seen[assetPath] {
				seen[assetPath] = true
				d.Assets = append(d.Assets, model.DocAsset{Token: t, Name: n.attr("name"), MIME: n.attr("mime"), Path: assetPath})
			}
		}
		for _, c := range n.kids {
			assets(c)
		}
	}
	for i := range d.Blocks {
		block := &d.Blocks[i]
		original := string(d.Markdown[block.Start:block.End])
		terminal := strings.HasSuffix(original, "\n")
		original = strings.TrimSuffix(original, "\n")
		roots, e := parseXML(block.RawXML)
		if e != nil || len(roots) != 1 {
			return d, unsupported("projection source")
		}
		n := roots[0]
		assets(n)
		frontMatter := i == 0 && o.ProjectionVersion == 3 && isYAMLFrontMatterNode(n)
		if frontMatter {
			original, e = yamlFrontMatterText(n)
			if e != nil {
				return d, e
			}
			block.Opaque = false
			block.Readable = false
			block.Kind = "pre"
			block.Hash = yamlFrontMatterHash([]byte(original))
		}
		if block.Opaque {
			original = readableXML(n)
			block.Readable = true
			block.Kind = n.tag
			// Hashes include all remote semantics even when display drops unsupported attributes.
			block.Hash = digest("opaque\x00" + opaqueSemantic(n))
			if n.tag == "table" {
				collectParagraphs(n, block.RemoteID, &block.Children)
			}
		}
		// Legacy list items stay adjacent; all other blocks have a blank separator.
		if i > 0 {
			sep := "\n\n"
			if i == 1 && o.ProjectionVersion == 3 && HasYAMLFrontMatterXML(d.Blocks[0].RawXML) && bytes.HasSuffix(md, []byte("\n")) {
				sep = "\n"
			}
			if strings.HasSuffix(block.Kind, "/li") && d.Blocks[i-1].Kind == block.Kind {
				sep = "\n"
			}
			md = append(md, sep...)
		}
		block.Start = len(md)
		md = append(md, original...)
		block.End = len(md)
		if i == len(d.Blocks)-1 && terminal && !frontMatter {
			md = append(md, '\n')
			block.End++
		}
	}
	d.Markdown = md
	return d, validate(md)
}

func collectParagraphs(n *xnode, parent string, out *[]model.Block) {
	for _, c := range n.kids {
		if c.tag == "p" && c.attr("id") != "" && len(elements(c)) == 0 && c.allowed("align") {
			*out = append(*out, model.Block{RemoteID: c.attr("id"), ParentID: parent, Kind: "p", Hash: hashNode("p", c), RawXML: bytes.Clone(c.raw)})
		}
		collectParagraphs(c, parent, out)
	}
}

// readableXML deliberately emits no remote IDs or media tokens. Unknown shapes
// keep readable descendants while their original raw XML remains in the sidecar.
func readableXML(n *xnode) string {
	if n.tag == "" {
		return esc(n.text)
	}
	if n.tag == "img" {
		return `<img src="` + esc(AssetPath(n.attr("src"), n.attr("name"), n.attr("mime"))) + `" alt="` + esc(n.attr("alt")) + `"/>`
	}
	tag := n.tag
	switch tag {
	case "p", "h1", "h2", "h3", "h4", "h5", "h6", "table", "thead", "tbody", "tr", "td", "th", "b", "em", "u", "s", "span", "a", "br", "pre", "code", "ul", "ol", "li", "blockquote", "hr", "div":
	case "colgroup":
		return ""
	default:
		tag = "div"
	}
	var out strings.Builder
	out.WriteString("<" + tag)
	for _, key := range []string{"colspan", "rowspan"} {
		if (tag == "td" || tag == "th") && n.attr(key) != "" {
			out.WriteString(fmt.Sprintf(` %s="%s"`, key, esc(n.attr(key))))
		}
	}
	if tag == "a" && safeURL(n.attr("href")) {
		out.WriteString(` href="` + esc(n.attr("href")) + `"`)
	}
	if tag == "br" || tag == "hr" {
		out.WriteString("/>")
		return out.String()
	}
	out.WriteByte('>')
	for _, c := range n.kids {
		out.WriteString(readableXML(c))
	}
	out.WriteString("</" + tag + ">")
	return out.String()
}
