package markdown

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPureParseConversionRulesExportsEffectiveImmutableSnapshotWithoutConfiguring(t *testing.T) {
	before := currentConversionRules()
	rules, err := PureParseConversionRules([]byte(`{"version":1,"rules":[{"id":"extra","source":"BEFORE","target":"AFTER","context":"paragraph","match":"literal"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if currentConversionRules() != before {
		t.Fatal("parse replaced active rules")
	}
	exported := rules.EffectiveDocumentJSON()
	var doc conversionRuleDocument
	if err := json.Unmarshal(exported, &doc); err != nil || len(doc.Rules) != 9 || doc.Rules[0].Target != "\n" || doc.Rules[8].Source != "BEFORE" {
		t.Fatalf("effective rules: %s, %v", exported, err)
	}
	exported[0] = '!'
	if !json.Valid(rules.EffectiveDocumentJSON()) {
		t.Fatal("caller mutated immutable snapshot")
	}
	roundTrip, err := PureParseConversionRules(rules.EffectiveDocumentJSON())
	if err != nil || roundTrip.Fingerprint() != rules.Fingerprint() {
		t.Fatalf("round trip changed fingerprint: %v", err)
	}
	if bytes.Equal(rules.EffectiveDocumentJSON(), DefaultConversionRulesJSON()) {
		t.Fatal("export omitted custom rule")
	}
}

func TestPureParseConversionRulesStrictValidation(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"version":1,"rules":[{"id":"x","source":"x","context":"paragraph","match":"literal"}]}`),
		[]byte(`{"version":1,"rules":[{"id":"x","source":"x","target":null,"context":"paragraph","match":"literal"}]}`),
		[]byte(`{"version":1,"rules":[{"id":"x","source":"x","target":"y","context":"paragraph","match":"literal","extra":1}]}`),
		bytes.Repeat([]byte(" "), 8*1024*1024+1),
	} {
		if _, err := PureParseConversionRules(input); err == nil {
			t.Fatal("accepted invalid document")
		}
	}
}
