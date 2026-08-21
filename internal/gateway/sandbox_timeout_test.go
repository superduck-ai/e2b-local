package gateway

import (
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
)

func TestSandboxTimeoutDefaultIsConsistentAcrossLayers(t *testing.T) {
	if DefaultSandboxTimeoutSeconds != 15 {
		t.Fatalf("expected E2B REST API default timeout 15, got %d", DefaultSandboxTimeoutSeconds)
	}
	if got := requestTimeout(nil); got != DefaultSandboxTimeoutSeconds {
		t.Fatalf("expected omitted request timeout %d, got %d", DefaultSandboxTimeoutSeconds, got)
	}

	createdAt := time.Now().UTC().Truncate(time.Second)
	expectedEndAt := defaultSandboxEndAt(createdAt)
	store := NewSandboxStore()
	record, err := store.Create(SandboxRecord{
		ID:        "sbx_default_timeout",
		CreatedAt: createdAt,
		State:     string(e2bapi.Running),
	})
	if err != nil {
		t.Fatalf("create sandbox record: %v", err)
	}
	if !record.EndAt.Equal(expectedEndAt) {
		t.Fatalf("expected store fallback EndAt %s, got %s", expectedEndAt, record.EndAt)
	}
	if record.OnTimeout != SandboxTimeoutActionKill {
		t.Fatalf("expected omitted timeout action to default to kill, got %q", record.OnTimeout)
	}

	restored, err := (&App{}).enrichRestoredSandboxRecord(SandboxRecord{
		ID:        "sbx_restored_default_timeout",
		CreatedAt: createdAt,
		State:     string(e2bapi.Running),
	})
	if err != nil {
		t.Fatalf("enrich restored sandbox: %v", err)
	}
	if !restored.EndAt.Equal(expectedEndAt) {
		t.Fatalf("expected restore fallback EndAt %s, got %s", expectedEndAt, restored.EndAt)
	}
	if restored.OnTimeout != SandboxTimeoutActionKill {
		t.Fatalf("expected restored legacy timeout action to default to kill, got %q", restored.OnTimeout)
	}

	if got := recordEndAt(SandboxRecord{CreatedAt: createdAt}); !got.Equal(expectedEndAt) {
		t.Fatalf("expected response fallback EndAt %s, got %s", expectedEndAt, got)
	}
}
