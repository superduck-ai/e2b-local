package gateway

import (
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
)

func TestGatewayManagementStoreTracksManagedResourcesInMemory(t *testing.T) {
	store := NewGatewayManagementStore()

	template := GatewayTemplate{
		Template: e2bapi.Template{
			BuildCount:  1,
			BuildID:     "build-1",
			BuildStatus: e2bapi.TemplateBuildStatusReady,
			CpuCount:    2,
			CreatedAt:   time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
			EnvdVersion: "99.99.99",
			MemoryMB:    1024,
			Names:       []string{"managed-template"},
			Public:      false,
			TemplateID:  "managed-template",
			UpdatedAt:   time.Date(2026, 6, 4, 0, 1, 0, 0, time.UTC),
		},
		ImageRef: "e2b-local/templates/managed-template:latest",
	}
	if _, err := store.UpsertTemplate(template, []string{"latest"}, []e2bapi.BuildLogEntry{
		{
			Level:     e2bapi.LogLevelInfo,
			Message:   "in-memory build",
			Timestamp: time.Date(2026, 6, 4, 0, 2, 0, 0, time.UTC),
		},
	}); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	uploadToken, err := store.CreateTemplateFileUpload("managed-template", "hash")
	if err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	stored, err := store.StoreTemplateFileUpload("managed-template", "hash", uploadToken, []byte("file-data"))
	if err != nil {
		t.Fatalf("store template file upload: %v", err)
	}
	if !stored {
		t.Fatal("expected template file upload to be stored")
	}
	if err := store.SetNodeStatus(localNodeID, e2bapi.NodeStatusDraining); err != nil {
		t.Fatalf("set node status: %v", err)
	}

	templates := store.ListManagedTemplates()
	if len(templates) != 1 || templates[0].TemplateID != "managed-template" || templates[0].ImageRef != template.ImageRef {
		t.Fatalf("unexpected managed templates: %#v", templates)
	}
	tags := store.ListTemplateTags("managed-template")
	if len(tags) != 1 || tags[0].Tag != "latest" {
		t.Fatalf("unexpected template tags: %#v", tags)
	}
	logs := store.TemplateBuildLogs("managed-template")
	if len(logs) != 1 || logs[0].Message != "in-memory build" {
		t.Fatalf("unexpected template build logs: %#v", logs)
	}
	file, ok := store.TemplateFile("managed-template", "hash")
	if !ok || string(file.Data) != "file-data" {
		t.Fatalf("unexpected template file: ok=%t file=%#v", ok, file)
	}
	if store.NodeStatus(localNodeID) != e2bapi.NodeStatusDraining {
		t.Fatalf("expected node status to be draining, got %q", store.NodeStatus(localNodeID))
	}
}
