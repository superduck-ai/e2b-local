package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"testing"
)

func TestSafeTemplateBuildContextTarNameRejectsTraversal(t *testing.T) {
	unsafeNames := []string{
		"../file.txt",
		"src/../../file.txt",
		"/absolute/file.txt",
		`..\file.txt`,
	}
	for _, name := range unsafeNames {
		if got, ok := SafeTemplateBuildContextTarName(name); ok {
			t.Fatalf("expected %q to be rejected, got %q", name, got)
		}
	}

	if got, ok := SafeTemplateBuildContextTarName("src/./file.txt"); !ok || got != "src/file.txt" {
		t.Fatalf("expected cleaned safe name, got %q ok=%t", got, ok)
	}
}

func TestValidateTemplateBuildFileArchiveRejectsTooManyEntries(t *testing.T) {
	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for i := 0; i <= MaxTemplateArchiveEntries(); i++ {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("src/file-%05d.txt", i),
			Mode: 0o644,
			Size: 0,
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	err := ValidateTemplateBuildFileArchive(TemplateBuildFile{
		Hash: "hash",
		Data: buf.Bytes(),
	})
	if err == nil {
		t.Fatal("expected too many archive entries to fail")
	}
	if status := GatewayErrorStatus(err, 0); status != 413 {
		t.Fatalf("expected too many entries status 413, got %d: %v", status, err)
	}
}

func TestValidateTemplateBuildFileArchiveRejectsUnsafePath(t *testing.T) {
	err := ValidateTemplateBuildFileArchive(TemplateBuildFile{
		Hash: "hash",
		Data: gzipTarBytes(t, map[string]string{"../file.txt": "data"}),
	})
	if err == nil {
		t.Fatal("expected unsafe archive path to fail")
	}
	if status := GatewayErrorStatus(err, 0); status != 400 {
		t.Fatalf("expected unsafe path status 400, got %d: %v", status, err)
	}
}
