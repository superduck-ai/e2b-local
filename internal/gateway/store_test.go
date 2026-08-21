package gateway

import (
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
)

func TestSandboxStoreKeepsRecordsInMemory(t *testing.T) {
	store := NewSandboxStore()

	createdAt := time.Now().UTC().Truncate(time.Second)
	endAt := createdAt.Add(10 * time.Minute)

	created, err := store.Create(SandboxRecord{
		ID:                   "sbx_memory",
		TemplateID:           "base",
		Metadata:             map[string]string{"source": "test"},
		EnvdVersion:          "99.99.99",
		RuntimeInfo:          SandboxRuntimeInfo{SandboxID: "sbx_memory", EnvdURL: "http://10.0.0.11:49983", MachineID: "machine-a", VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/data"}}},
		CreatedAt:            createdAt,
		EndAt:                endAt,
		State:                string(e2bapi.Running),
		InternetAccessPolicy: InternetAccessAllowed,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.EnvdURL != "http://10.0.0.11:49983" {
		t.Fatalf("expected envd URL to be filled from runtime info, got %q", created.EnvdURL)
	}

	record, ok := store.Get("sbx_memory")
	if !ok {
		t.Fatal("expected sandbox to be available")
	}
	if record.Metadata["source"] != "test" || record.RuntimeInfo.MachineID != "machine-a" || record.EnvdURL != "http://10.0.0.11:49983" {
		t.Fatalf("unexpected record: %#v", record)
	}
	if len(record.RuntimeInfo.VolumeMounts) != 1 || record.RuntimeInfo.VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("expected volume mounts to round-trip, got %#v", record.RuntimeInfo.VolumeMounts)
	}
}

func TestSandboxStoreClonesRecordReferences(t *testing.T) {
	store := NewSandboxStore()

	created, err := store.Create(SandboxRecord{
		ID:                   "sbx_clone",
		Alias:                "original",
		Metadata:             map[string]string{"source": "original"},
		InternetAccessPolicy: InternetAccessAllowed,
		RuntimeInfo: SandboxRuntimeInfo{
			VolumeMounts:   []VolumeMount{{Name: "original"}},
			PublishedPorts: []SandboxPortMapping{{HostPort: 8080}},
		},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	created.Alias = "mutated result"
	created.Metadata["source"] = "mutated result"
	created.RuntimeInfo.VolumeMounts[0].Name = "mutated result"
	created.RuntimeInfo.PublishedPorts[0].HostPort = 9090
	created.InternetAccessPolicy = InternetAccessDenied

	stored, ok := store.Get("sbx_clone")
	if !ok {
		t.Fatal("expected sandbox to be available")
	}
	if stored.Alias != "original" ||
		stored.Metadata["source"] != "original" ||
		stored.RuntimeInfo.VolumeMounts[0].Name != "original" ||
		stored.RuntimeInfo.PublishedPorts[0].HostPort != 8080 ||
		stored.InternetAccessPolicy != InternetAccessAllowed {
		t.Fatalf("stored record was mutated through an external reference: %#v", stored)
	}
}

func TestSandboxStoreUpsertMergesCachedRecordFields(t *testing.T) {
	store := NewSandboxStore()

	if _, err := store.Create(SandboxRecord{
		ID:         "sbx_cache",
		TemplateID: "base",
		RuntimeInfo: SandboxRuntimeInfo{
			SandboxID: "sbx_cache",
			EnvdURL:   "http://10.0.0.11:49983",
			MachineID: "machine-a",
		},
		State:     string(e2bapi.Running),
		OnTimeout: SandboxTimeoutActionPause,
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	updated, err := store.Upsert(SandboxRecord{
		ID:    "sbx_cache",
		State: string(e2bapi.Paused),
		RuntimeInfo: SandboxRuntimeInfo{
			SandboxID: "sbx_cache",
		},
	})
	if err != nil {
		t.Fatalf("upsert sandbox: %v", err)
	}
	if updated.RuntimeInfo.MachineID != "machine-a" {
		t.Fatalf("expected cached runtime info to be preserved, got %#v", updated.RuntimeInfo)
	}
	if updated.State != string(e2bapi.Paused) {
		t.Fatalf("expected updated state to win, got %q", updated.State)
	}
	if updated.OnTimeout != SandboxTimeoutActionPause {
		t.Fatalf("expected omitted timeout action to preserve cached pause, got %q", updated.OnTimeout)
	}

	updated, err = store.Upsert(SandboxRecord{
		ID:        "sbx_cache",
		OnTimeout: SandboxTimeoutActionKill,
	})
	if err != nil {
		t.Fatalf("upsert explicit timeout action: %v", err)
	}
	if updated.OnTimeout != SandboxTimeoutActionKill {
		t.Fatalf("expected explicit kill timeout action to win, got %q", updated.OnTimeout)
	}
}
