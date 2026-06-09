//go:build darwin

package orbstackbackend

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCreateVolumeStoresMetadataInNewXAttrWithoutToken(t *testing.T) {
	hostPath := t.TempDir()
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			VolumeHostPath: hostPath,
		},
		vmClient: &fakeVMClient{},
		logger:   log.New(io.Discard, "", 0),
		now:      time.Now,
		newID: func() string {
			return "vol-created"
		},
	}

	created, err := runtime.CreateVolume(context.Background(), "data")
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	resolved, err := findVolumeByID(runtime.cfg, created.VolumeID)
	if err != nil {
		t.Fatalf("resolve volume: %v", err)
	}

	data, err := readVolumeMetadataXAttr(resolved.HostDir, volumeMetadataXAttrName)
	if err != nil {
		t.Fatalf("read new metadata xattr: %v", err)
	}
	if bytes.Contains(data, []byte(`"Token"`)) {
		t.Fatalf("expected xattr metadata without token, got %s", data)
	}
	if bytes.Contains(data, []byte(`com.e2b.gateway.volume-meta`)) {
		t.Fatalf("unexpected legacy xattr marker in metadata %s", data)
	}
	if _, err := readVolumeMetadataXAttr(resolved.HostDir, legacyVolumeMetadataXAttrName); !isMissingVolumeMetadataXAttr(err) {
		t.Fatalf("expected legacy xattr key to be absent, got err=%v", err)
	}
}

func TestReadVolumeMetadataMigratesLegacyXAttrKeyAndDropsToken(t *testing.T) {
	hostPath := t.TempDir()
	hostDir := volumeBaseDir(OrbstackRuntimeConfig{VolumeHostPath: hostPath}, "legacy-data")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("mkdir host dir: %v", err)
	}

	legacyData := []byte(`{"VolumeID":"vol-legacy","Name":"legacy-data","Token":"token-legacy"}`)
	if err := unix.Setxattr(hostDir, legacyVolumeMetadataXAttrName, legacyData, 0); err != nil {
		t.Fatalf("set legacy xattr: %v", err)
	}

	volume, err := readVolumeMetadata(hostDir)
	if err != nil {
		t.Fatalf("read volume metadata: %v", err)
	}
	if volume.VolumeID != "vol-legacy" || volume.Name != "legacy-data" {
		t.Fatalf("unexpected migrated volume metadata: %#v", volume)
	}

	data, err := readVolumeMetadataXAttr(hostDir, volumeMetadataXAttrName)
	if err != nil {
		t.Fatalf("read migrated xattr: %v", err)
	}
	if bytes.Contains(data, []byte(`"Token"`)) {
		t.Fatalf("expected migrated xattr without token, got %s", data)
	}
	if _, err := readVolumeMetadataXAttr(hostDir, legacyVolumeMetadataXAttrName); !isMissingVolumeMetadataXAttr(err) {
		t.Fatalf("expected legacy xattr key to be removed, got err=%v", err)
	}
}
