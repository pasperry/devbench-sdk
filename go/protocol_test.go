package devbench

import (
	"encoding/json"
	"slices"
	"testing"
)

// The protocol version this SDK declares is the one every SDK and ingest
// agree on (docs/server-sdk/protocol-version.md, "Protocol version"). The wire tests
// (capture_test, panic_test, report_handled_test, direct_test) assert that
// each payload carries ProtocolVersion as `v`; this holds the constant to
// the shared vectors, so an SDK speaking a version ingest refuses cannot pass.
func TestProtocolVersion_IsTheVectorsCurrent(t *testing.T) {
	var vectors struct {
		Current   *int  `json:"current"`
		Supported []int `json:"supported"`
		Refused   []int `json:"refused"`
	}
	if err := json.Unmarshal(readVectors(t, "protocol_vectors.json"), &vectors); err != nil {
		t.Fatalf("parse protocol_vectors.json: %v", err)
	}
	if vectors.Current == nil {
		t.Fatal("protocol_vectors.json has no current")
	}
	if ProtocolVersion != *vectors.Current {
		t.Errorf("ProtocolVersion = %d, the vectors' current is %d", ProtocolVersion, *vectors.Current)
	}
	if !slices.Contains(vectors.Supported, ProtocolVersion) {
		t.Errorf("ProtocolVersion %d is not in supported %v", ProtocolVersion, vectors.Supported)
	}
	if slices.Contains(vectors.Refused, ProtocolVersion) {
		t.Errorf("ProtocolVersion %d is in refused %v", ProtocolVersion, vectors.Refused)
	}
}
