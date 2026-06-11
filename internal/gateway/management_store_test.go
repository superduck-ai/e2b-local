package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	archive := gzipTarBytes(t, map[string]string{"src/file.txt": "file-data"})
	stored, err := store.StoreTemplateFileUpload("managed-template", "hash", uploadToken, archive)
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
	if !ok {
		t.Fatalf("unexpected template file: ok=%t file=%#v", ok, file)
	}
	data, err := file.ReadAll()
	if err != nil {
		t.Fatalf("read template file: %v", err)
	}
	if !bytes.Equal(data, archive) {
		t.Fatalf("unexpected template file data: %d bytes", len(data))
	}
	if store.NodeStatus(localNodeID) != e2bapi.NodeStatusDraining {
		t.Fatalf("expected node status to be draining, got %q", store.NodeStatus(localNodeID))
	}
}

func TestGatewayManagementStoreValidatesRecognizedTemplateUploadHash(t *testing.T) {
	store := NewGatewayManagementStore()
	archive := gzipTarBytes(t, map[string]string{"src/file.txt": "file-data"})
	sum := sha256.Sum256(archive)
	hash := hex.EncodeToString(sum[:])

	uploadToken, err := store.CreateTemplateFileUpload("managed-template", hash)
	if err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	stored, err := store.StoreTemplateFileUpload("managed-template", hash, uploadToken, archive)
	if err != nil {
		t.Fatalf("store template file upload: %v", err)
	}
	if !stored {
		t.Fatal("expected template file upload to be stored")
	}

	badHash := "0000000000000000000000000000000000000000000000000000000000000000"
	badUploadToken, err := store.CreateTemplateFileUpload("managed-template", badHash)
	if err != nil {
		t.Fatalf("create bad upload token: %v", err)
	}
	stored, err = store.StoreTemplateFileUpload("managed-template", badHash, badUploadToken, archive)
	if err == nil {
		t.Fatal("expected mismatched template file upload hash to fail")
	}
	if !stored {
		t.Fatal("expected matching upload token before hash validation failure")
	}
	if status := GatewayErrorStatus(err, 0); status != 400 {
		t.Fatalf("expected bad hash status 400, got %d: %v", status, err)
	}
	if store.TemplateFilePresent("managed-template", badHash) {
		t.Fatal("expected mismatched template file upload not to be stored")
	}
}

func TestGatewayManagementStoreTreatsTemplateFileHashAsImmutable(t *testing.T) {
	store := NewGatewayManagementStore()
	archive := gzipTarBytes(t, map[string]string{"src/file.txt": "file-data"})

	uploadToken, err := store.CreateTemplateFileUpload("managed-template", "hash")
	if err != nil {
		t.Fatalf("create upload token: %v", err)
	}
	stored, err := store.StoreTemplateFileUpload("managed-template", "hash", uploadToken, archive)
	if err != nil {
		t.Fatalf("store template file upload: %v", err)
	}
	if !stored {
		t.Fatal("expected template file upload to be stored")
	}

	heldFile, ok := store.TemplateFile("managed-template", "hash")
	if !ok {
		t.Fatal("expected stored template file")
	}

	sameUploadToken, err := store.CreateTemplateFileUpload("managed-template", "hash")
	if err != nil {
		t.Fatalf("create second upload token: %v", err)
	}
	stored, err = store.StoreTemplateFileUpload("managed-template", "hash", sameUploadToken, archive)
	if err != nil {
		t.Fatalf("store duplicate template file upload: %v", err)
	}
	if !stored {
		t.Fatal("expected duplicate template file upload to be accepted")
	}

	currentFile, ok := store.TemplateFile("managed-template", "hash")
	if !ok {
		t.Fatal("expected template file to remain stored")
	}
	if currentFile.Path != heldFile.Path {
		t.Fatalf("expected duplicate upload to keep existing path, got %q want %q", currentFile.Path, heldFile.Path)
	}
	heldData, err := heldFile.ReadAll()
	if err != nil {
		t.Fatalf("read held template file after duplicate upload: %v", err)
	}
	if !bytes.Equal(heldData, archive) {
		t.Fatalf("unexpected held template file data: %d bytes", len(heldData))
	}

	differentArchive := gzipTarBytes(t, map[string]string{"src/file.txt": "different"})
	differentUploadToken, err := store.CreateTemplateFileUpload("managed-template", "hash")
	if err != nil {
		t.Fatalf("create different upload token: %v", err)
	}
	stored, err = store.StoreTemplateFileUpload("managed-template", "hash", differentUploadToken, differentArchive)
	if err == nil {
		t.Fatal("expected different content for existing template file hash to fail")
	}
	if !stored {
		t.Fatal("expected matching upload token before immutable hash failure")
	}
	if status := GatewayErrorStatus(err, 0); status != 409 {
		t.Fatalf("expected immutable hash status 409, got %d: %v", status, err)
	}

	currentFile, ok = store.TemplateFile("managed-template", "hash")
	if !ok {
		t.Fatal("expected template file to remain stored")
	}
	if currentFile.Path != heldFile.Path {
		t.Fatalf("expected rejected upload to keep existing path, got %q want %q", currentFile.Path, heldFile.Path)
	}
	heldData, err = heldFile.ReadAll()
	if err != nil {
		t.Fatalf("read held template file after rejected upload: %v", err)
	}
	if !bytes.Equal(heldData, archive) {
		t.Fatalf("unexpected held template file data after rejected upload: %d bytes", len(heldData))
	}
}
