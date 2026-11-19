package markdown

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

const contextTable = `<table id="%s"><colgroup><col width="120"/></colgroup><thead><tr><th><p>状态</p></th></tr></thead><tbody><tr><td><p>待填写</p></td></tr></tbody></table>`

func contextDocument(t *testing.T, version int) model.DocView {
	t.Helper()
	raw := `<title id="doc">台账</title><h1 id="g">广州</h1><h2 id="gs">资源</h2>` + fmt.Sprintf(contextTable, "gz") + `<p id="gp">说明相同</p><h1 id="s">银川</h1><h2 id="ss">资源</h2>` + fmt.Sprintf(contextTable, "silver") + `<p id="sp">说明相同</p><p id="tail">独立尾段</p>`
	v, err := DecodeWithOptions([]byte(raw), "1", Options{ProjectionVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func replaceLastContext(body []byte, old, new string) []byte {
	i := bytes.LastIndex(body, []byte(old))
	if i < 0 {
		panic("fixture missing text")
	}
	out := bytes.Clone(body[:i])
	out = append(out, new...)
	return append(out, body[i+len(old):]...)
}
func TestContextAlignmentKeepsIndependentEqualTables(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			current := contextDocument(t, version)
			for _, change := range []string{"tail", "silver table", "silver delete"} {
				t.Run(change, func(t *testing.T) {
					desired := bytes.Clone(current.Markdown)
					id, kind := "tail", model.OperationBlockReplace
					switch change {
					case "tail":
						desired = bytes.Replace(desired, []byte("独立尾段"), []byte("尾段已更新"), 1)
					case "silver table":
						desired = replaceLastContext(desired, "待填写", "已部署")
						id = "silver"
					case "silver delete":
						for _, b := range current.Blocks {
							if b.RemoteID == "silver" {
								desired = append(bytes.Clone(desired[:b.Start]), desired[b.End:]...)
								break
							}
						}
						id = "silver"
						kind = model.OperationBlockDelete
					}
					result, err := PlanMergedStep(desired, current)
					if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
						t.Fatalf("legitimate context rejected: reason=%s operations=%d err=%v", result.ConflictReason, len(result.Operations), err)
					}
					op := result.Operations[0]
					if op.BlockID != id || op.Kind != kind {
						t.Fatal("wrong independent cloud object", op)
					}
					var payload model.OperationPayload
					_ = json.Unmarshal(op.Payload, &payload)
					if payload.ExplicitResolution {
						t.Fatal("automatic match borrowed human authority")
					}
				})
			}
		})
	}
}
func TestContextAlignmentRecursesWithinRepeatedHeadings(t *testing.T) {
	// The repeated subheading is uniquely scoped by the distinct parent headings.
	current := contextDocument(t, 2)
	desired := replaceLastContext(current.Markdown, "说明相同", "银川说明已更新")
	result, err := PlanMergedStep(desired, current)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 || result.Operations[0].BlockID != "sp" {
		t.Fatal("parent-scoped repeated heading rejected", result.ConflictReason, result.Operations, err)
	}
}
func TestContextAlignmentStillRejectsIdentityGuesses(t *testing.T) {
	for _, tc := range []struct {
		name, xml string
		desired   []byte
	}{
		{"one indistinguishable copy removed", `<title id="doc">T</title><p id="a">same</p><p id="b">same</p><p id="end">end</p>`, []byte("same\n\nend\n")},
		{"unique blocks reordered", `<title id="doc">T</title><p id="a">aaa</p><p id="b">bbb</p><p id="c">ccc</p>`, []byte("bbb\n\naaa\n\nccc\n")},
		{"same block moved between sections", `<title id="doc">T</title><h1 id="a">A</h1><p id="one">same</p><h1 id="b">B</h1><p id="two">same</p><h1 id="c">C</h1>`, []byte("# A\n\n# B\n\nsame\n\n# C\n\nsame\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, version := range []int{1, 2} {
				current, err := DecodeWithOptions([]byte(tc.xml), "1", Options{ProjectionVersion: version})
				if err != nil {
					t.Fatal(err)
				}
				result, err := PlanMergedStep(tc.desired, current)
				if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
					t.Fatal("unsafe fallback crossed identity boundary", result.ConflictReason, result.Operations, err)
				}
			}
		})
	}
}
func TestContextAlignmentRetainsConcurrentTableConflict(t *testing.T) {
	before := contextDocument(t, 2)
	local := replaceLastContext(before.Markdown, "待填写", "本地修改")
	raw := strings.Replace(string(before.XML), `<table id="silver">`, `<table id="silver">`, 1)
	index := strings.Index(raw, `<table id="silver">`)
	raw = raw[:index] + strings.Replace(raw[index:], "待填写", "云端修改", 1)
	remote, err := Decode([]byte(raw), "2")
	if err != nil {
		t.Fatal(err)
	}
	result, err := PlanStepWithBase(before.Markdown, local, before, remote)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatal("concurrent edit overwritten", result.ConflictReason, result.Operations, err)
	}
}
func TestContextAlignmentPreservesRichResourceWhileEditingElsewhere(t *testing.T) {
	current := readable(t, `<h1 id="a">A</h1><p id="one">same</p><table id="rich"><tbody><tr><td><p id="x">first</p><p id="y">second</p><img id="img" src="resource"/></td></tr></tbody></table><h1 id="b">B</h1><p id="two">same</p><p id="end">tail</p>`)
	desired := bytes.Replace(current.Markdown, []byte("tail"), []byte("updated"), 1)
	result, err := PlanMergedStep(desired, current)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 || result.Operations[0].BlockID != "end" {
		t.Fatal("unchanged rich object blocked independent edit", result.ConflictReason, result.Operations, err)
	}
}

func contextApplyTop(t *testing.T, before model.DocView, op model.Operation, target string) model.DocView {
	t.Helper()
	var p model.OperationPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		t.Fatal(err)
	}
	replacement := p.XML
	if op.Kind == model.OperationBlockDelete {
		replacement = ""
	} else {
		at := strings.IndexByte(replacement, '>')
		if at < 0 {
			t.Fatal("missing XML", replacement)
		}
		replacement = replacement[:at] + ` id="changed"` + replacement[at:]
	}
	raw := string(before.XML)
	found := false
	for _, block := range before.Blocks {
		if block.RemoteID == target {
			raw = strings.Replace(raw, string(block.RawXML), replacement, 1)
			found = true
			break
		}
	}
	if !found {
		t.Fatal("missing target", target)
	}
	result, err := DecodeWithOptions([]byte(raw), "2", Options{ProjectionVersion: before.ProjectionVersion})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestContextMatchedTableReadbackUsesExactIDAndWholeSnapshot(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		t.Run(fmt.Sprint(deletion), func(t *testing.T) {
			before := contextDocument(t, 2)
			desired := replaceLastContext(before.Markdown, "待填写", "已部署")
			if deletion {
				for _, block := range before.Blocks {
					if block.RemoteID == "silver" {
						desired = append(bytes.Clone(before.Markdown[:block.Start]), before.Markdown[block.End:]...)
					}
				}
			}
			result, err := PlanMergedStep(desired, before)
			if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
				t.Fatal(result.ConflictReason, err)
			}
			op, err := ProjectOperationHash(result.Operations[0], before)
			if err != nil {
				t.Fatal(err)
			}
			correct := contextApplyTop(t, before, op, "silver")
			if err = VerifyOperation(before, correct, op); err != nil {
				t.Fatal("correct independent table effect rejected", err)
			}
			wrong := contextApplyTop(t, before, op, "gz")
			if err = VerifyOperation(before, wrong, op); err == nil {
				t.Fatal("wrong equal-content table accepted")
			}
			drift, err := DecodeWithOptions(bytes.Replace(before.XML, []byte("独立尾段"), []byte("云端新版本"), 1), before.Revision, Options{ProjectionVersion: before.ProjectionVersion})
			if err != nil {
				t.Fatal(err)
			}
			driftAfter := contextApplyTop(t, drift, op, "silver")
			if err = VerifyOperation(drift, driftAfter, op); err == nil {
				t.Fatal("operation rebound to a different captured document")
			}
			var legacy model.OperationPayload
			_ = json.Unmarshal(op.Payload, &legacy)
			legacy.ContextHash = ""
			oldOp := op
			oldOp.Payload, _ = json.Marshal(legacy)
			if err = VerifyOperation(before, correct, oldOp); err == nil {
				t.Fatal("legacy ambiguous journal lost its conservative guard")
			}
		})
	}
}

func TestContextListReplacementHashUsesItsParent(t *testing.T) {
	for _, parent := range []string{"ul", "ol"} {
		for _, version := range []int{1, 2, 3} {
			t.Run(fmt.Sprintf("%s-v%d", parent, version), func(t *testing.T) {
				raw := fmt.Sprintf(`<title id="doc">T</title><%s><li id="a">first</li><li id="b">second</li></%s>`, parent, parent)
				before, err := DecodeWithOptions([]byte(raw), "1", Options{ProjectionVersion: version})
				if err != nil {
					t.Fatal(err)
				}
				desired := bytes.Replace(before.Markdown, []byte("second"), []byte("updated"), 1)
				result, err := PlanMergedStep(desired, before)
				if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
					t.Fatal(result.ConflictReason, err)
				}
				op, err := ProjectOperationHash(result.Operations[0], before)
				if err != nil {
					t.Fatal(err)
				}
				after := contextApplyTop(t, before, op, "b")
				if err = VerifyOperation(before, after, op); err != nil {
					t.Fatal("list fragment projected without its containing list", err)
				}
			})
		}
	}
}

func TestContextNestedSameTextReadbackKeepsContainingTable(t *testing.T) {
	before := readable(t, `<h1 id="g">广州</h1><table id="gt"><tbody><tr><td><p id="gf">first</p><p id="gp">same</p></td></tr></tbody></table><h1 id="s">银川</h1><table id="st"><tbody><tr><td><p id="sf">first</p><p id="sp">same</p></td></tr></tbody></table>`)
	desired := replaceLastContext(before.Markdown, ">same<", ">updated<")
	result, err := PlanMergedStep(desired, before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 || result.Operations[0].BlockID != "sp" {
		t.Fatal("wrong nested target", result.ConflictReason, err)
	}
	op, err := ProjectOperationHash(result.Operations[0], before)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Decode(bytes.Replace(before.XML, []byte(`<p id="sp">same</p>`), []byte(`<p id="changed">updated</p>`), 1), "2")
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyOperation(before, after, op); err != nil {
		t.Fatal("correct nested independent paragraph rejected", err)
	}
	wrong, err := Decode(bytes.Replace(before.XML, []byte(`<p id="gp">same</p>`), []byte(`<p id="changed">updated</p>`), 1), "2")
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyOperation(before, wrong, op); err == nil {
		t.Fatal("different parent table accepted")
	}
}

func TestContextAlignmentAcceptedMappingsAreInjectiveAndComplete(t *testing.T) {
	sequences := [][]atom{{}}
	for size := 1; size <= 4; size++ {
		for bits := 0; bits < 1<<size; bits++ {
			var atoms []atom
			for i := 0; i < size; i++ {
				h := "a"
				if bits&(1<<i) != 0 {
					h = "b"
				}
				atoms = append(atoms, atom{Block: model.Block{Kind: "p", Hash: h}})
			}
			sequences = append(sequences, atoms)
		}
	}
	for _, a := range sequences {
		for _, b := range sequences {
			for _, projected := range []bool{false, true} {
				match, err := alignContext(a, b, projected)
				if err != nil {
					continue
				}
				seen := map[int]bool{}
				last := -1
				for _, j := range match.index {
					if j < 0 {
						continue
					}
					if j >= len(b) || j <= last || seen[j] {
						t.Fatal("non-injective or reordered identity mapping")
					}
					last = j
					seen[j] = true
				}
				for gap, indices := range match.inserts {
					if gap < 0 || gap > len(a) {
						t.Fatal("invalid insertion boundary")
					}
					for _, j := range indices {
						if j < 0 || j >= len(b) || seen[j] {
							t.Fatal("ambiguous duplicate target coverage")
						}
						seen[j] = true
					}
				}
				if len(seen) != len(b) {
					t.Fatal("target content omitted from mapping")
				}
			}
		}
	}
}
func TestContextAlignmentBoundsUnanchoredProjectedWork(t *testing.T) {
	a, b := make([]atom, 2500), make([]atom, 2500)
	for i := range a {
		a[i] = atom{Block: model.Block{Kind: "p", Hash: fmt.Sprintf("old%d", i)}}
		b[i] = atom{Block: model.Block{Kind: "p", Hash: fmt.Sprintf("new%d", i)}}
	}
	if _, err := alignContext(a, b, true); !errors.Is(err, errAlignmentBudget) {
		t.Fatal("unbounded projected matrix", err)
	}
}

func TestContextAlignmentRejectsSurvivingCloudIDsMovedToOtherSections(t *testing.T) {
	before := contextDocument(t, 2)
	raw := strings.ReplaceAll(string(before.XML), `id="gz"`, `id="temporary"`)
	raw = strings.ReplaceAll(raw, `id="silver"`, `id="gz"`)
	raw = strings.ReplaceAll(raw, `id="temporary"`, `id="silver"`)
	current, err := Decode([]byte(raw), "2")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Markdown, current.Markdown) {
		t.Fatal("fixture must hide move in Markdown")
	}
	desired := replaceLastContext(before.Markdown, "待填写", "已部署")
	result, err := PlanStepWithBase(before.Markdown, desired, before, current)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatal("old ID movement ignored", result.ConflictReason, len(result.Operations), err)
	}
}
func TestContextAlignmentRejectsCrossRegionExchangeHiddenAsReplacements(t *testing.T) {
	current := readable(t, `<h1 id="a">A</h1><p id="x1">X</p><h1 id="b">B</h1><p id="y1">Y</p><h1 id="c">C</h1><p id="x2">X</p><h1 id="d">D</h1><p id="y2">Y</p>`)
	desired := []byte("# A\n\nY\n\n# B\n\nX\n\n# C\n\nX\n\n# D\n\nY\n")
	result, err := PlanMergedStep(desired, current)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatal("cross-section exchange disguised as edits", result.ConflictReason, len(result.Operations), err)
	}
}
func TestContextAlignmentHardFailureAnywhereBlocksAutomaticFallback(t *testing.T) {
	for _, tc := range []struct{ raw, desired string }{
		{`<h1 id="a">A</h1><p id="old">old</p><h1 id="b">B</h1><p id="s1">same</p><p id="s2">same</p><h1 id="c">C</h1><p id="p1">p1</p><p id="p2">p2</p>`, "# A\n\nupdated\n\n# B\n\nsame\n\n# C\n\nq1\n\nq2\n"},
		{`<h1 id="a">A</h1><p id="keep">keep</p><h1 id="b">B</h1><p id="s1">same</p><p id="s2">same</p><h1 id="c">C</h1>`, "# A\n\nnew\n\nkeep\n\n# B\n\nsame\n\n# C\n"},
	} {
		current := readable(t, tc.raw)
		result, err := PlanMergedStep([]byte(tc.desired), current)
		if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
			t.Fatal("fallback wrote before later hard ambiguity", result.ConflictReason, len(result.Operations), err)
		}
	}
}
func TestContextOperationProofCannotBeRetargeted(t *testing.T) {
	before := contextDocument(t, 2)
	desired := replaceLastContext(before.Markdown, "待填写", "已部署")
	result, err := PlanMergedStep(desired, before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 {
		t.Fatal(result.ConflictReason, err)
	}
	op := result.Operations[0]
	op.BlockID = "gz"
	if _, err = ProjectOperationHash(op, before); err == nil {
		t.Fatal("context proof transferred to another table")
	}
	after := contextApplyTop(t, before, op, "gz")
	if err = VerifyOperation(before, after, op); err == nil {
		t.Fatal("retargeted persisted proof accepted")
	}
}
func TestContextProjectedInsertionUsesLinearMemory(t *testing.T) {
	next := make([]atom, 8000)
	for i := range next {
		next[i] = atom{Block: model.Block{Kind: "p", Hash: fmt.Sprintf("unique-%d", i)}}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	result, err := alignProjected(nil, next)
	runtime.ReadMemStats(&after)
	if err != nil || len(result.inserts[0]) != len(next) {
		t.Fatal("bounded insertion rejected", err)
	}
	if used := after.TotalAlloc - before.TotalAlloc; used > 32<<20 {
		t.Fatalf("insertion allocated %d bytes", used)
	}
}

func TestContextNestedAdjacentEqualParagraphsRemainAmbiguous(t *testing.T) {
	before := readable(t, `<h1 id="h">Section</h1><table id="table"><tbody><tr><td><p id="a">same</p><p id="b">same</p></td></tr></tbody></table>`)
	desired := bytes.Replace(before.Markdown, []byte(">same<"), []byte(">updated<"), 1)
	result, err := PlanMergedStep(desired, before)
	if err != nil || result.ConflictReason == "" || len(result.Operations) != 0 {
		t.Fatal("nested paragraph selected only by ordinal", result.ConflictReason, len(result.Operations), err)
	}
}

func TestContextNestedSameTextInDifferentCellsHasStructuralScope(t *testing.T) {
	before := readable(t, `<h1 id="h">Section</h1><table id="table"><tbody><tr><td><p id="a">same</p></td><td><p id="b">same</p></td></tr></tbody></table>`)
	desired := bytes.Replace(before.Markdown, []byte(">same<"), []byte(">updated<"), 1)
	result, err := PlanMergedStep(desired, before)
	if err != nil || result.ConflictReason != "" || len(result.Operations) != 1 || result.Operations[0].BlockID != "a" {
		t.Fatal("different cell identity lost", result.ConflictReason, err)
	}
	op, err := ProjectOperationHash(result.Operations[0], before)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Decode(bytes.Replace(before.XML, []byte(`<p id="a">same</p>`), []byte(`<p id="changed">updated</p>`), 1), "2")
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyOperation(before, after, op); err != nil {
		t.Fatal(err)
	}
}
