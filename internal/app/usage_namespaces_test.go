package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageCountersIsolatedByGateway(t *testing.T) {
	usage := &UsageTracker{}
	acct := newAccount("shared", "shared-key", true)

	usage.RecorderWithGateway(GatewayCmdcode, acct.ID, "").Record(10, 20, 0, 0)
	usage.RecorderForGateway(GatewayOpencode, acct, "").Record(1, 2, 3, 4)
	// Legacy recorders default to the cmdcode namespace.
	usage.Recorder(acct.ID, "").Record(100, 100, 0, 0)
	usage.ForAccount(acct).Record(0, 0, 0, 7)

	cc := usage.AccountUsageFor(GatewayCmdcode, acct.ID)
	if cc.Requests != 3 || cc.PromptTokens != 110 || cc.CompletionTokens != 120 || cc.CacheWriteTokens != 7 {
		t.Fatalf("cmdcode usage = %+v", cc)
	}
	zen := usage.AccountUsageFor(GatewayOpencode, acct.ID)
	if zen.Requests != 1 || zen.PromptTokens != 1 || zen.CompletionTokens != 2 || zen.CacheReadTokens != 3 || zen.CacheWriteTokens != 4 {
		t.Fatalf("opencode usage = %+v", zen)
	}
	if got := usage.AccountUsageFor(GatewayZen, acct.ID); got != zen {
		t.Fatalf("zen usage = %+v, want %+v", got, zen)
	}
	if got := usage.AccountUsage(acct.ID); got != cc {
		t.Fatalf("legacy AccountUsage = %+v, want cmdcode %+v", got, cc)
	}
	snap := usage.Snapshot()
	if len(snap.Accounts) != 2 {
		t.Fatalf("accounts = %v, want 2 namespaced entries", snap.Accounts)
	}
	if snap.TotalRequests != 4 || snap.PromptTokens != 111 || snap.CompletionTokens != 122 {
		t.Fatalf("totals = %+v", snap)
	}
}

func TestUsageMigratesOpencodeNamespaceToZen(t *testing.T) {
	dir := t.TempDir()
	oldFile := usageFile
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { usageFile = oldFile })
	data := `{"accounts":{"opencode:a12345678":{"requests":1,"prompt_tokens":2,"completion_tokens":3,"cache_read_tokens":0,"cache_write_tokens":0}}}`
	if err := os.WriteFile(usageFile, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	loaded := loadUsage()
	if got := loaded.AccountUsageFor(GatewayZen, "a12345678"); got.Requests != 1 || got.PromptTokens != 2 || got.CompletionTokens != 3 {
		t.Fatalf("zen usage = %+v", got)
	}
	if _, ok := loaded.Snapshot().Accounts["opencode:a12345678"]; ok {
		t.Fatalf("legacy opencode namespace was retained: %+v", loaded.Snapshot().Accounts)
	}
}

func TestUsageLegacySnapshotLoadsAsCmdcode(t *testing.T) {
	dir := t.TempDir()
	oldFile := usageFile
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { usageFile = oldFile })

	legacy := `{"total_requests":2,"prompt_tokens":30,"completion_tokens":40,` +
		`"cache_read_tokens":0,"cache_write_tokens":0,` +
		`"accounts":{"a12345678":{"requests":2,"prompt_tokens":30,"completion_tokens":40,` +
		`"cache_read_tokens":0,"cache_write_tokens":0}}}`
	if err := os.WriteFile(usageFile, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	loaded := loadUsage()
	got := loaded.AccountUsage("a12345678")
	if got.Requests != 2 || got.PromptTokens != 30 || got.CompletionTokens != 40 {
		t.Fatalf("migrated usage = %+v", got)
	}
	if zen := loaded.AccountUsageFor(GatewayOpencode, "a12345678"); zen.Requests != 0 {
		t.Fatalf("opencode usage must start empty = %+v", zen)
	}
	if snap := loaded.Snapshot(); len(snap.Accounts) != 1 {
		t.Fatalf("accounts = %v, want a single namespaced entry", snap.Accounts)
	} else if _, ok := snap.Accounts["cmdcode:a12345678"]; !ok {
		t.Fatalf("accounts = %v, want cmdcode:a12345678 key", snap.Accounts)
	}

	// Saving persists the namespaced key; reloading must not duplicate.
	if err := loaded.save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Accounts map[string]UsageSnapshotEntry `json:"accounts"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Accounts) != 1 || persisted.Accounts["cmdcode:a12345678"].Requests != 2 {
		t.Fatalf("persisted accounts = %v, want only cmdcode:a12345678", persisted.Accounts)
	}

	reloaded := loadUsage()
	if got := reloaded.AccountUsage("a12345678"); got.Requests != 2 || got.PromptTokens != 30 {
		t.Fatalf("reloaded usage = %+v", got)
	}
	if snap := reloaded.Snapshot(); len(snap.Accounts) != 1 {
		t.Fatalf("reloaded accounts = %v, want no duplication", snap.Accounts)
	}
}

func TestUsageLegacyAndNamespacedDuplicatesMerge(t *testing.T) {
	dir := t.TempDir()
	oldFile := usageFile
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { usageFile = oldFile })

	id := accountID("dup-key")
	snap := persistedUsage{}
	snap.Accounts = map[string]UsageSnapshotEntry{
		id:                        {Requests: 1, PromptTokens: 10, CompletionTokens: 10},
		GatewayCmdcode + ":" + id: {Requests: 2, PromptTokens: 20, CompletionTokens: 20},
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usageFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	loaded := loadUsage()
	got := loaded.AccountUsageFor(GatewayCmdcode, id)
	if got.Requests != 3 || got.PromptTokens != 30 || got.CompletionTokens != 30 {
		t.Fatalf("merged usage = %+v, want summed values", got)
	}
	if snap := loaded.Snapshot(); len(snap.Accounts) != 1 {
		t.Fatalf("accounts = %v, want a single merged entry", snap.Accounts)
	}
}

func TestUsageMoveAccountIsNamespaced(t *testing.T) {
	usage := &UsageTracker{}
	oldID := accountID("move-old-key")
	newID := accountID("move-new-key")

	usage.RecorderWithGateway(GatewayCmdcode, oldID, "").Record(10, 10, 0, 0)
	usage.RecorderWithGateway(GatewayOpencode, oldID, "").Record(1, 2, 0, 0)

	usage.MoveAccountFor(GatewayOpencode, oldID, newID)

	if got := usage.AccountUsageFor(GatewayOpencode, oldID); got.Requests != 0 {
		t.Fatalf("old opencode counters = %+v, want empty", got)
	}
	if moved := usage.AccountUsageFor(GatewayOpencode, newID); moved.Requests != 1 || moved.PromptTokens != 1 || moved.CompletionTokens != 2 {
		t.Fatalf("moved opencode usage = %+v", moved)
	}
	if got := usage.AccountUsageFor(GatewayCmdcode, oldID); got.Requests != 1 || got.PromptTokens != 10 {
		t.Fatalf("cmdcode usage must be untouched = %+v", got)
	}
	if got := usage.AccountUsageFor(GatewayCmdcode, newID); got.Requests != 0 {
		t.Fatalf("cmdcode new id must stay empty = %+v", got)
	}

	// Legacy MoveAccount keeps operating on the cmdcode namespace.
	usage.MoveAccount(oldID, newID)
	if got := usage.AccountUsageFor(GatewayCmdcode, oldID); got.Requests != 0 {
		t.Fatalf("old cmdcode counters = %+v, want empty", got)
	}
	if got := usage.AccountUsageFor(GatewayCmdcode, newID); got.Requests != 1 || got.PromptTokens != 10 {
		t.Fatalf("moved cmdcode usage = %+v", got)
	}
	if got := usage.AccountUsageFor(GatewayOpencode, newID); got.Requests != 1 || got.PromptTokens != 1 {
		t.Fatalf("opencode usage changed by legacy move = %+v", got)
	}
}

func TestUsageDropAccountIsNamespaced(t *testing.T) {
	usage := &UsageTracker{}
	id := accountID("drop-key")
	usage.RecorderWithGateway(GatewayCmdcode, id, "").Record(3, 3, 0, 0)
	usage.RecorderWithGateway(GatewayOpencode, id, "").Record(5, 5, 0, 0)

	usage.DropAccountFor(GatewayOpencode, id)

	if got := usage.AccountUsageFor(GatewayOpencode, id); got.Requests != 0 {
		t.Fatalf("opencode counters = %+v, want dropped", got)
	}
	if got := usage.AccountUsageFor(GatewayCmdcode, id); got.Requests != 1 {
		t.Fatalf("cmdcode counters must survive = %+v", got)
	}
}
