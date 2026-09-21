package web

import "testing"

func TestAgentApplyNeedsFullReloadAfterOlderRevisionFailed(t *testing.T) {
	// Snapshot A changed a line, was written to the node database, then its
	// reload failed. Snapshot B changes only users, so the database comparison
	// reports no line change; the durable pending marker must still force a
	// full reconciliation instead of acknowledging B after ReloadUsers alone.
	if !agentApplyNeedsFullReload("revision-a", false, true) {
		t.Fatal("a pending older revision must force full reload for a newer user-only snapshot")
	}
	if agentApplyNeedsFullReload("", false, true) {
		t.Fatal("without a pending marker, a running core with only user changes may use hot reload")
	}
	if !agentApplyNeedsFullReload("", false, false) {
		t.Fatal("a stopped core must be fully reconciled before ACK")
	}
}
