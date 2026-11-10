package model

import (
	"encoding/json"
	"testing"
)

func TestOperationJSONCompatibility(t *testing.T) {
	var op Operation
	if err := json.Unmarshal([]byte(`{"kind":"update","block_id":"block","payload":{"text":"x"}}`), &op); err != nil {
		t.Fatal(err)
	}
	if op.Route != "" || op.CapabilityID != "" || op.ClientToken != "" {
		t.Fatal("legacy operation acquired dispatch authority")
	}
	op.ID = "journal"
	op.Route = "verified-route"
	op.CapabilityID = "cap"
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["capability_id"]) != `"cap"` || string(got["block_id"]) != `"block"` || string(got["payload"]) != `{"text":"x"}` {
		t.Fatalf("operation fields lost: %s", data)
	}
}
func TestSnapshotOpaqueRevision(t *testing.T) {
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"revision":"opaque/v12","token":"resource-token"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Revision != "opaque/v12" || s.Token != "resource-token" {
		t.Fatalf("resource identity changed: %+v", s)
	}
}

func TestJobJournalOperationIDsJSON(t *testing.T) {
	var job Job
	if err := json.Unmarshal([]byte(`{"operation_ids":["journal-1","journal-2"]}`), &job); err != nil {
		t.Fatal(err)
	}
	if len(job.OperationIDs) != 2 || job.OperationIDs[0] != "journal-1" {
		t.Fatalf("journal operation references lost: %+v", job)
	}
}
