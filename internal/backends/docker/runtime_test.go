package dockerbackend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestDockerRuntimeContainerCommandStartsEnvdDirectly(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	runtime := &DockerRuntime{cfg: cfg}

	cmd := runtime.containerCommand("")
	want := []string{"-isnotfc", "-port", "49983"}

	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("expected envd command %#v, got %#v", want, cmd)
	}

	cmd = runtime.containerCommand("python main.py")
	want = []string{"-isnotfc", "-port", "49983", "-cmd", "python main.py"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("expected envd start command %#v, got %#v", want, cmd)
	}
}

func TestDockerHostSupportsLocalBindMounts(t *testing.T) {
	for _, tt := range []struct {
		host string
		want bool
	}{
		{host: "unix:///var/run/docker.sock", want: true},
		{host: "UNIX:///var/run/docker.sock", want: true},
		{host: "npipe:////./pipe/docker_engine", want: true},
		{host: "NPIPE:////./pipe/docker_engine", want: true},
		{host: "tcp://127.0.0.1:2375", want: true},
		{host: "TCP://127.0.0.1:2375", want: true},
		{host: "tcp://localhost:2375", want: true},
		{host: "tcp://192.0.2.10:2375", want: false},
		{host: "ssh://example.com", want: false},
	} {
		t.Run(tt.host, func(t *testing.T) {
			if got := dockerHostSupportsLocalBindMounts(tt.host); got != tt.want {
				t.Fatalf("expected %t for %q, got %t", tt.want, tt.host, got)
			}
		})
	}
}

func TestDockerReadyCommandRunsInShell(t *testing.T) {
	cmd := dockerReadyCommand("  curl -fsS http://127.0.0.1:8000/ready  ")
	want := []string{"/bin/sh", "-lc", "curl -fsS http://127.0.0.1:8000/ready"}

	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("expected ready command %#v, got %#v", want, cmd)
	}
}

func TestNormalizeDockerPublishedPortsSortsDeduplicatesAndSkipsEnvd(t *testing.T) {
	ports := normalizeDockerPublishedPorts([]int{5001, 0, 5000, 5000, dockerEnvdPort, 65536})
	want := []int{5000, 5001}

	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("expected normalized ports %#v, got %#v", want, ports)
	}
}

func TestDockerPortBindingsPublishesBusinessPortsOnConfiguredHostIP(t *testing.T) {
	envdPort := dockerEnvdNatPort()
	bindings := dockerPortBindings(envdPort, []int{5000}, "0.0.0.0")

	if got := bindings[envdPort][0].HostIP; got != dockerEnvdHostIP {
		t.Fatalf("expected envd binding host IP %q, got %q", dockerEnvdHostIP, got)
	}
	if got := bindings[dockerTCPNatPort(5000)][0].HostIP; got != "0.0.0.0" {
		t.Fatalf("expected business port host IP 0.0.0.0, got %q", got)
	}
}

func TestDockerPublishedPortsFromBindingsSkipsEnvd(t *testing.T) {
	mappings := dockerPublishedPortsFromBindings(map[nat.Port][]nat.PortBinding{
		dockerEnvdNatPort(): []nat.PortBinding{{
			HostIP:   dockerEnvdHostIP,
			HostPort: "38122",
		}},
		dockerTCPNatPort(5000): []nat.PortBinding{{
			HostIP:   "0.0.0.0",
			HostPort: "38123",
		}},
	})

	if len(mappings) != 1 {
		t.Fatalf("expected one published business port, got %#v", mappings)
	}
	if mappings[0].ContainerPort != 5000 || mappings[0].HostPort != 38123 || mappings[0].Protocol != "tcp" {
		t.Fatalf("unexpected published port mapping: %#v", mappings[0])
	}
}

func TestDockerRuntimeLocalVolumeLifecycleUsesManagedDirectories(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	unmanagedDir := filepath.Join(cfg.VolumeHostPath, "unmanaged")
	if err := os.MkdirAll(unmanagedDir, 0o755); err != nil {
		t.Fatalf("create unmanaged dir: %v", err)
	}
	linkedDir := filepath.Join(cfg.VolumeHostPath, "linked")
	linkedTarget := filepath.Join(t.TempDir(), "linked-target")
	if err := os.MkdirAll(linkedTarget, 0o755); err != nil {
		t.Fatalf("create linked target: %v", err)
	}
	symlinkSupported := os.Symlink(linkedTarget, linkedDir) == nil

	created, err := runtime.CreateVolume(context.Background(), "managed")
	if err != nil {
		t.Fatalf("create local volume: %v", err)
	}
	if created.VolumeID != "managed" || created.Name != "managed" {
		t.Fatalf("unexpected created volume: %#v", created)
	}
	metadataPath, err := runtime.localVolumeMetadataPath("managed")
	if err != nil {
		t.Fatalf("resolve managed metadata path: %v", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("expected managed marker file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.VolumeHostPath, "managed", dockerLocalVolumeMetadataFile)); !os.IsNotExist(err) {
		t.Fatalf("expected managed marker outside content directory, stat err=%v", err)
	}

	listed, err := runtime.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("list local volumes: %v", err)
	}
	if len(listed) != 1 || listed[0].VolumeID != "managed" {
		t.Fatalf("expected only managed volume, got %#v", listed)
	}
	if symlinkSupported {
		if _, err := runtime.GetVolume(context.Background(), "linked"); !errdefs.IsNotFound(err) {
			t.Fatalf("expected symlink volume directory to be ignored, got %v", err)
		}
	}

	resolved, err := runtime.GetVolume(context.Background(), "managed")
	if err != nil {
		t.Fatalf("get local volume: %v", err)
	}
	if resolved.VolumeID != "managed" {
		t.Fatalf("unexpected resolved volume: %#v", resolved)
	}

	deleted, err := runtime.DeleteVolume(context.Background(), "unmanaged")
	if err != nil {
		t.Fatalf("delete unmanaged volume should not fail: %v", err)
	}
	if deleted {
		t.Fatal("expected unmanaged directory to be ignored")
	}
	if _, err := os.Stat(unmanagedDir); err != nil {
		t.Fatalf("unmanaged directory should remain: %v", err)
	}
	if symlinkSupported {
		deleted, err := runtime.DeleteVolume(context.Background(), "linked")
		if err != nil {
			t.Fatalf("delete symlink volume should not fail: %v", err)
		}
		if deleted {
			t.Fatal("expected symlink volume directory to be ignored")
		}
		if _, err := os.Lstat(linkedDir); err != nil {
			t.Fatalf("symlink volume directory should remain: %v", err)
		}
	}

	deleted, err = runtime.DeleteVolume(context.Background(), "managed")
	if err != nil {
		t.Fatalf("delete managed volume: %v", err)
	}
	if !deleted {
		t.Fatal("expected managed volume to be deleted")
	}
	if _, err := os.Stat(filepath.Join(cfg.VolumeHostPath, "managed")); !os.IsNotExist(err) {
		t.Fatalf("expected managed directory removed, stat err=%v", err)
	}
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("expected managed metadata removed, stat err=%v", err)
	}
}

func TestDockerRuntimeMountsUseBindMountsForLocalVolumes(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	if _, err := runtime.CreateVolume(context.Background(), "skills"); err != nil {
		t.Fatalf("create local volume: %v", err)
	}

	volumeMounts, dockerMounts, err := runtime.mounts(context.Background(), []VolumeMount{
		{Name: "skills", Path: "/mnt/skills"},
	}, "/tmp/envd")
	if err != nil {
		t.Fatalf("build mounts: %v", err)
	}
	if len(volumeMounts) != 1 || volumeMounts[0].VolumeID != "skills" || volumeMounts[0].Path != "/mnt/skills" {
		t.Fatalf("unexpected normalized volume mounts: %#v", volumeMounts)
	}
	if len(dockerMounts) != 2 {
		t.Fatalf("expected envd and volume mounts, got %#v", dockerMounts)
	}
	volumeMount := dockerMounts[1]
	if volumeMount.Type != mount.TypeBind {
		t.Fatalf("expected bind mount, got %#v", volumeMount)
	}
	if want := filepath.Join(cfg.VolumeHostPath, "skills"); volumeMount.Source != want {
		t.Fatalf("expected bind source %q, got %q", want, volumeMount.Source)
	}
	if volumeMount.Target != "/mnt/skills" {
		t.Fatalf("expected bind target /mnt/skills, got %q", volumeMount.Target)
	}
}

func TestDockerRuntimeMountsAutoCreateMissingLocalVolumes(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	volumeMounts, dockerMounts, err := runtime.mounts(context.Background(), []VolumeMount{
		{Name: "user-data", Path: "/mnt/user-data"},
	}, "/tmp/envd")
	if err != nil {
		t.Fatalf("build mounts: %v", err)
	}
	if len(volumeMounts) != 1 || volumeMounts[0].VolumeID != "user-data" || volumeMounts[0].Name != "user-data" {
		t.Fatalf("unexpected normalized volume mounts: %#v", volumeMounts)
	}
	if len(dockerMounts) != 2 || dockerMounts[1].Type != mount.TypeBind {
		t.Fatalf("expected envd plus bind volume mounts, got %#v", dockerMounts)
	}
	if want := filepath.Join(cfg.VolumeHostPath, "user-data"); dockerMounts[1].Source != want {
		t.Fatalf("expected bind source %q, got %q", want, dockerMounts[1].Source)
	}
	metadataPath, err := runtime.localVolumeMetadataPath("user-data")
	if err != nil {
		t.Fatalf("resolve auto-created metadata path: %v", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("expected auto-created managed marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.VolumeHostPath, "user-data", dockerLocalVolumeMetadataFile)); !os.IsNotExist(err) {
		t.Fatalf("expected auto-created marker outside content directory, stat err=%v", err)
	}

	listed, err := runtime.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("list local volumes: %v", err)
	}
	if len(listed) != 1 || listed[0].VolumeID != "user-data" {
		t.Fatalf("expected auto-created user-data in managed list, got %#v", listed)
	}
}

func TestDockerRuntimeMountsRejectUnmanagedLocalVolumeDirectory(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	if err := os.MkdirAll(filepath.Join(cfg.VolumeHostPath, "user-data"), 0o755); err != nil {
		t.Fatalf("create unmanaged user-data dir: %v", err)
	}

	if _, _, err := runtime.mounts(context.Background(), []VolumeMount{
		{Name: "user-data", Path: "/mnt/user-data"},
	}, "/tmp/envd"); err == nil {
		t.Fatal("expected unmanaged local volume directory to fail")
	}
	if _, err := os.Stat(filepath.Join(cfg.VolumeHostPath, "user-data")); err != nil {
		t.Fatalf("unmanaged directory should remain: %v", err)
	}
}

func TestDockerVolumeMountsFromMountPointsRestoresBindVolumes(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	mounts := []dockertypes.MountPoint{
		{
			Type:        mount.TypeBind,
			Source:      filepath.Join(cfg.VolumeHostPath, "skills"),
			Destination: "/mnt/skills",
		},
		{
			Type:        mount.TypeBind,
			Source:      "/tmp/envd",
			Destination: dockerEnvdPath,
		},
		{
			Type:        mount.TypeVolume,
			Name:        "legacy",
			Destination: "/mnt/legacy",
		},
	}

	got := runtime.dockerVolumeMountsFromMountPoints(mounts)
	want := []VolumeMount{
		{Name: "skills", Path: "/mnt/skills", VolumeID: "skills", MountPath: "/mnt/skills"},
		{Name: "legacy", Path: "/mnt/legacy", VolumeID: "legacy", MountPath: "/mnt/legacy"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected restored mounts: %#v", got)
	}
}

func TestDockerRuntimeVolumeContentReadWriteAndStat(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	volume, err := runtime.CreateVolume(context.Background(), "skills")
	if err != nil {
		t.Fatalf("create local volume: %v", err)
	}

	mode := 0o640
	written, err := runtime.WriteVolumeFile(context.Background(), volume.VolumeID, "/manifest.json", strings.NewReader(`{"skills":[]}`), gateway.VolumeWriteOptions{
		Force: true,
		Mode:  &mode,
	})
	if err != nil {
		t.Fatalf("write volume file: %v", err)
	}
	if written.Type != "file" || written.Path != "/manifest.json" || written.Mode != mode {
		t.Fatalf("unexpected written stat: %#v", written)
	}
	if goruntime.GOOS != "windows" {
		if written.UID != os.Getuid() || written.GID != os.Getgid() {
			t.Fatalf("expected current uid/gid in written stat, got %#v", written)
		}
		if written.Atime.IsZero() || written.Ctime.IsZero() {
			t.Fatalf("expected platform atime/ctime in written stat, got %#v", written)
		}
	}

	if _, err := runtime.WriteVolumeFile(context.Background(), volume.VolumeID, "/manifest.json", strings.NewReader(`{"skills":["new"]}`), gateway.VolumeWriteOptions{}); err == nil {
		t.Fatal("expected write without force to reject existing file")
	} else if status := gatewayErrorStatus(err, http.StatusInternalServerError); status != http.StatusConflict {
		t.Fatalf("expected existing file conflict, got status %d err %v", status, err)
	}

	body, err := runtime.ReadVolumeFile(context.Background(), volume.VolumeID, "manifest.json")
	if err != nil {
		t.Fatalf("read volume file: %v", err)
	}
	data, err := io.ReadAll(body)
	if closeErr := body.Close(); closeErr != nil {
		t.Fatalf("close volume file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read volume body: %v", err)
	}
	if string(data) != `{"skills":[]}` {
		t.Fatalf("unexpected volume file body %q", string(data))
	}

	stat, err := runtime.GetVolumePathInfo(context.Background(), volume.VolumeID, "manifest.json")
	if err != nil {
		t.Fatalf("stat volume file: %v", err)
	}
	if stat.Path != "/manifest.json" || stat.Size != int64(len(`{"skills":[]}`)) {
		t.Fatalf("unexpected stat: %#v", stat)
	}

	dirOpts := gateway.VolumeWriteOptions{}
	if goruntime.GOOS != "windows" {
		uid := os.Getuid()
		gid := os.Getgid()
		dirOpts.UID = &uid
		dirOpts.GID = &gid
	}
	dirStat, err := runtime.CreateVolumeDir(context.Background(), volume.VolumeID, "nested", dirOpts)
	if err != nil {
		t.Fatalf("create volume dir: %v", err)
	}
	if dirStat.Type != "directory" || dirStat.Path != "/nested" {
		t.Fatalf("unexpected dir stat: %#v", dirStat)
	}
	if goruntime.GOOS != "windows" && (dirStat.UID != os.Getuid() || dirStat.GID != os.Getgid()) {
		t.Fatalf("expected requested uid/gid in directory stat, got %#v", dirStat)
	}

	entries, err := runtime.ListVolumeDir(context.Background(), volume.VolumeID, "/", 1)
	if err != nil {
		t.Fatalf("list volume dir: %v", err)
	}
	if len(entries) != 2 || entries[0].Path != "/manifest.json" || entries[1].Path != "/nested" {
		t.Fatalf("unexpected volume dir entries: %#v", entries)
	}

	if _, err := runtime.GetVolumePathInfo(context.Background(), volume.VolumeID, "missing.json"); !errdefs.IsNotFound(err) {
		t.Fatalf("expected missing path to be not found, got %v", err)
	}
}

func TestDockerRuntimeVolumeContentForceWriteKeepsOldContentOnCopyFailure(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	volume, err := runtime.CreateVolume(context.Background(), "skills")
	if err != nil {
		t.Fatalf("create local volume: %v", err)
	}
	if _, err := runtime.WriteVolumeFile(context.Background(), volume.VolumeID, "manifest.json", strings.NewReader("old-content"), gateway.VolumeWriteOptions{Force: true}); err != nil {
		t.Fatalf("seed volume file: %v", err)
	}

	_, err = runtime.WriteVolumeFile(context.Background(), volume.VolumeID, "manifest.json", &failingReader{data: []byte("partial")}, gateway.VolumeWriteOptions{Force: true})
	if err == nil {
		t.Fatal("expected failing reader to fail")
	}

	body, err := runtime.ReadVolumeFile(context.Background(), volume.VolumeID, "manifest.json")
	if err != nil {
		t.Fatalf("read volume file after failed write: %v", err)
	}
	data, err := io.ReadAll(body)
	if closeErr := body.Close(); closeErr != nil {
		t.Fatalf("close volume file: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read volume body: %v", err)
	}
	if string(data) != "old-content" {
		t.Fatalf("expected old content to survive failed force write, got %q", string(data))
	}
}

func TestDockerRuntimeVolumeContentWriteAnchorsParentDirectory(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("renaming an open directory is not portable on Windows")
	}
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}
	volume, err := runtime.CreateVolume(context.Background(), "skills")
	if err != nil {
		t.Fatalf("create local volume: %v", err)
	}
	if _, err := runtime.CreateVolumeDir(context.Background(), volume.VolumeID, "nested", gateway.VolumeWriteOptions{}); err != nil {
		t.Fatalf("create nested volume directory: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	writeErr := make(chan error, 1)
	go func() {
		_, err := runtime.WriteVolumeFile(context.Background(), volume.VolumeID, "nested/file.txt", &gatedReader{
			data:    []byte("anchored"),
			started: started,
			release: release,
		}, gateway.VolumeWriteOptions{Force: true})
		writeErr <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for volume write to start")
	}
	volumeDir := filepath.Join(cfg.VolumeHostPath, volume.VolumeID)
	originalParent := filepath.Join(volumeDir, "nested")
	movedParent := filepath.Join(volumeDir, "nested-original")
	outsideDir := t.TempDir()
	if err := os.Rename(originalParent, movedParent); err != nil {
		t.Fatalf("move open volume parent: %v", err)
	}
	if err := os.Symlink(outsideDir, originalParent); err != nil {
		t.Fatalf("replace volume parent with symlink: %v", err)
	}
	close(release)
	released = true

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("complete anchored volume write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for volume write to finish")
	}
	data, err := os.ReadFile(filepath.Join(movedParent, "file.txt"))
	if err != nil {
		t.Fatalf("read file from anchored parent: %v", err)
	}
	if string(data) != "anchored" {
		t.Fatalf("unexpected anchored file content %q", data)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected outside file not to be created, stat err=%v", err)
	}
}

func TestDockerRuntimeVolumeContentWriteRejectsSymlinkTarget(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}
	volume, err := runtime.CreateVolume(context.Background(), "skills")
	if err != nil {
		t.Fatalf("create local volume: %v", err)
	}
	volumeDir := filepath.Join(cfg.VolumeHostPath, volume.VolumeID)
	targetPath := filepath.Join(volumeDir, "target.txt")
	if err := os.WriteFile(targetPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed symlink target: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(volumeDir, "link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	linkStat, err := runtime.GetVolumePathInfo(context.Background(), volume.VolumeID, "link.txt")
	if err != nil {
		t.Fatalf("stat in-volume symlink: %v", err)
	}
	if linkStat.Type != "symlink" || linkStat.Target != "target.txt" {
		t.Fatalf("unexpected in-volume symlink stat: %#v", linkStat)
	}

	_, err = runtime.WriteVolumeFile(context.Background(), volume.VolumeID, "link.txt", strings.NewReader("replacement"), gateway.VolumeWriteOptions{Force: true})
	if err == nil {
		t.Fatal("expected symlink target write to fail")
	}
	if status := gatewayErrorStatus(err, http.StatusInternalServerError); status != http.StatusBadRequest {
		t.Fatalf("expected bad request for symlink target, got status %d err %v", status, err)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read symlink target after rejected write: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("expected symlink target to remain unchanged, got %q", data)
	}
}

func TestDockerRuntimeLocalVolumeMigratesLegacyMetadataOutsideContentDir(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	legacyDir := filepath.Join(cfg.VolumeHostPath, "legacy")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("create legacy volume dir: %v", err)
	}
	legacyMetadata := []byte(`{"volume_id":"legacy","name":"legacy","managed":true,"created_at":"2026-07-09T00:00:00Z"}` + "\n")
	if err := os.WriteFile(filepath.Join(legacyDir, dockerLocalVolumeMetadataFile), legacyMetadata, 0o644); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}

	volume, err := runtime.GetVolume(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("get legacy volume: %v", err)
	}
	if volume.VolumeID != "legacy" {
		t.Fatalf("unexpected legacy volume: %#v", volume)
	}

	metadataPath, err := runtime.localVolumeMetadataPath("legacy")
	if err != nil {
		t.Fatalf("resolve legacy metadata path: %v", err)
	}
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("expected migrated metadata file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, dockerLocalVolumeMetadataFile)); !os.IsNotExist(err) {
		t.Fatalf("expected legacy content marker removed, stat err=%v", err)
	}
}

func TestDockerRuntimeVolumeContentRejectsReservedMetadataPath(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	if _, err := runtime.CreateVolume(context.Background(), "skills"); err != nil {
		t.Fatalf("create local volume: %v", err)
	}

	for _, tt := range []struct {
		name string
		call func() error
	}{
		{
			name: "write metadata",
			call: func() error {
				_, err := runtime.WriteVolumeFile(context.Background(), "skills", dockerLocalVolumeMetadataFile, strings.NewReader("{}"), gateway.VolumeWriteOptions{Force: true})
				return err
			},
		},
		{
			name: "write metadata with different casing",
			call: func() error {
				_, err := runtime.WriteVolumeFile(context.Background(), "skills", strings.ToUpper(dockerLocalVolumeMetadataFile), strings.NewReader("{}"), gateway.VolumeWriteOptions{Force: true})
				return err
			},
		},
		{
			name: "stat metadata",
			call: func() error {
				_, err := runtime.GetVolumePathInfo(context.Background(), "skills", dockerLocalVolumeMetadataFile)
				return err
			},
		},
		{
			name: "read metadata",
			call: func() error {
				body, err := runtime.ReadVolumeFile(context.Background(), "skills", dockerLocalVolumeMetadataFile)
				if body != nil {
					_ = body.Close()
				}
				return err
			},
		},
		{
			name: "create under metadata",
			call: func() error {
				_, err := runtime.CreateVolumeDir(context.Background(), "skills", dockerLocalVolumeMetadataFile+"/nested", gateway.VolumeWriteOptions{})
				return err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("expected reserved path to fail")
			}
			if status := gatewayErrorStatus(err, http.StatusInternalServerError); status != http.StatusBadRequest {
				t.Fatalf("expected bad request for reserved path, got status %d err %v", status, err)
			}
		})
	}

	if _, err := runtime.GetVolume(context.Background(), "skills"); err != nil {
		t.Fatalf("reserved path attempts should not corrupt volume metadata: %v", err)
	}
}

func TestDockerRuntimeVolumeContentRejectsExcessiveListDepth(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	if _, err := runtime.CreateVolume(context.Background(), "skills"); err != nil {
		t.Fatalf("create local volume: %v", err)
	}

	_, err := runtime.ListVolumeDir(context.Background(), "skills", "/", maxDockerVolumeListDepth+1)
	if err == nil {
		t.Fatal("expected excessive depth to fail")
	}
	if status := gatewayErrorStatus(err, http.StatusInternalServerError); status != http.StatusBadRequest {
		t.Fatalf("expected bad request for excessive depth, got status %d err %v", status, err)
	}
}

func TestDockerRuntimeVolumeContentRejectsUnsafePaths(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker
	cfg.VolumeHostPath = t.TempDir()
	runtime := &DockerRuntime{cfg: cfg}

	if _, err := runtime.CreateVolume(context.Background(), "skills"); err != nil {
		t.Fatalf("create local volume: %v", err)
	}

	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "relative traversal", path: "../x"},
		{name: "absolute traversal", path: "/../../x"},
		{name: "nul", path: "a\x00b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runtime.WriteVolumeFile(context.Background(), "skills", tt.path, strings.NewReader("x"), gateway.VolumeWriteOptions{Force: true})
			if err == nil {
				t.Fatal("expected unsafe path to fail")
			}
			if status := gatewayErrorStatus(err, http.StatusInternalServerError); status != http.StatusBadRequest {
				t.Fatalf("expected bad request for unsafe path, got status %d err %v", status, err)
			}
		})
	}

	outsideDir := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("create outside dir: %v", err)
	}
	escapeLink := filepath.Join(cfg.VolumeHostPath, "skills", "escape")
	if err := os.Symlink(outsideDir, escapeLink); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	for _, tt := range []struct {
		name string
		call func() error
	}{
		{
			name: "write",
			call: func() error {
				_, err := runtime.WriteVolumeFile(context.Background(), "skills", "escape/file.txt", strings.NewReader("x"), gateway.VolumeWriteOptions{Force: true})
				return err
			},
		},
		{
			name: "read",
			call: func() error {
				body, err := runtime.ReadVolumeFile(context.Background(), "skills", "escape/file.txt")
				if body != nil {
					_ = body.Close()
				}
				return err
			},
		},
		{
			name: "stat",
			call: func() error {
				_, err := runtime.GetVolumePathInfo(context.Background(), "skills", "escape")
				return err
			},
		},
		{
			name: "list",
			call: func() error {
				_, err := runtime.ListVolumeDir(context.Background(), "skills", "escape", 1)
				return err
			},
		},
		{
			name: "mkdir",
			call: func() error {
				_, err := runtime.CreateVolumeDir(context.Background(), "skills", "escape/nested", gateway.VolumeWriteOptions{})
				return err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("expected symlink escape to fail")
			}
			if status := gatewayErrorStatus(err, http.StatusInternalServerError); status != http.StatusBadRequest {
				t.Fatalf("expected bad request for symlink escape, got status %d err %v", status, err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected outside file not to be created, stat err=%v", err)
	}
}

func TestDockerRuntimeEnvdBinaryForPlatformUsesBundledBinary(t *testing.T) {
	runtime := &DockerRuntime{}

	amd64Binary, err := runtime.envdBinaryForPlatform(ocispec.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("select amd64 envd binary: %v", err)
	}
	if !strings.HasSuffix(amd64Binary, filepath.Join("envd-bin", "envd-linux-amd64")) {
		t.Fatalf("expected amd64 bundled envd binary, got %q", amd64Binary)
	}

	arm64Binary, err := runtime.envdBinaryForPlatform(ocispec.Platform{OS: "linux", Architecture: "aarch64"})
	if err != nil {
		t.Fatalf("select arm64 envd binary: %v", err)
	}
	if !strings.HasSuffix(arm64Binary, filepath.Join("envd-bin", "envd-linux-arm64")) {
		t.Fatalf("expected arm64 bundled envd binary, got %q", arm64Binary)
	}
}

func TestDockerRuntimeEnvdBinaryForPlatformUsesOverride(t *testing.T) {
	runtime := &DockerRuntime{cfg: DockerRuntimeConfig{EnvdBinary: "/tmp/custom-envd"}}

	envdBinary, err := runtime.envdBinaryForPlatform(ocispec.Platform{OS: "windows", Architecture: "s390x"})
	if err != nil {
		t.Fatalf("select override envd binary: %v", err)
	}
	if envdBinary != "/tmp/custom-envd" {
		t.Fatalf("expected override envd binary, got %q", envdBinary)
	}
}

func TestDockerRuntimeEnvdBinaryForPlatformRejectsUnsupportedArchitecture(t *testing.T) {
	runtime := &DockerRuntime{}

	if _, err := runtime.envdBinaryForPlatform(ocispec.Platform{OS: "linux", Architecture: "s390x"}); err == nil {
		t.Fatal("expected unsupported architecture to fail")
	}
}

func TestReadyCommandFailureIncludesExitCodeAndOutput(t *testing.T) {
	message := readyCommandFailure(7, "not ready\n")
	if message != "exit code 7: not ready" {
		t.Fatalf("unexpected ready command failure %q", message)
	}

	message = readyCommandFailure(1, "")
	if message != "exit code 1" {
		t.Fatalf("unexpected empty ready command failure %q", message)
	}
}

func TestDockerBuildContextContainsDockerfile(t *testing.T) {
	reader, err := dockerBuildContext("FROM alpine:3.20\nRUN echo ok\n")
	if err != nil {
		t.Fatalf("create build context: %v", err)
	}

	tarReader := tar.NewReader(reader)
	header, err := tarReader.Next()
	if err != nil {
		t.Fatalf("read tar header: %v", err)
	}
	if header.Name != "Dockerfile" {
		t.Fatalf("expected Dockerfile entry, got %q", header.Name)
	}

	data, err := io.ReadAll(tarReader)
	if err != nil {
		t.Fatalf("read dockerfile entry: %v", err)
	}
	if string(data) != "FROM alpine:3.20\nRUN echo ok\n" {
		t.Fatalf("unexpected dockerfile content %q", string(data))
	}
	if _, err := tarReader.Next(); err != io.EOF {
		t.Fatalf("expected only one tar entry, got err=%v", err)
	}
}

func TestDockerBuildContextIncludesUploadedTemplateFiles(t *testing.T) {
	archive := gzipTarBytes(t, map[string]string{
		"src/file.txt": "hello",
	})
	reader, err := dockerBuildContext("FROM alpine:3.20\n", TemplateBuildFile{
		TemplateID: "template",
		Hash:       "hash",
		Data:       archive,
	})
	if err != nil {
		t.Fatalf("create build context: %v", err)
	}

	tarReader := tar.NewReader(reader)
	entries := map[string]string{}
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read tar entry: %v", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("read tar data: %v", err)
		}
		entries[header.Name] = string(data)
	}

	if entries["Dockerfile"] != "FROM alpine:3.20\n" {
		t.Fatalf("expected Dockerfile in build context, got %#v", entries)
	}
	if entries["src/file.txt"] != "hello" {
		t.Fatalf("expected uploaded file in build context, got %#v", entries)
	}
}

func TestDockerBuildLogEntriesDecodeStreamAndErrors(t *testing.T) {
	input := strings.NewReader(`{"stream":"Step 1/1 : FROM alpine\n"}{"status":"pulling","progress":"1/1"}{"errorDetail":{"message":"build failed"}}`)

	logs, err := dockerBuildLogEntries(input)
	if err == nil {
		t.Fatal("expected docker build error")
	}
	if !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("expected build failure message, got %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected three log entries, got %#v", logs)
	}
	if logs[0].Message != "Step 1/1 : FROM alpine" || logs[0].Level != e2bapi.LogLevelInfo {
		t.Fatalf("unexpected first log entry: %#v", logs[0])
	}
	if logs[2].Message != "build failed" || logs[2].Level != e2bapi.LogLevelError {
		t.Fatalf("unexpected error log entry: %#v", logs[2])
	}
}

func TestDockerLogEntriesParseTimestampsAndFilters(t *testing.T) {
	cursor := time.Date(2026, 6, 4, 0, 0, 1, 0, time.UTC).UnixMilli()
	search := "keep"
	input := strings.Join([]string{
		"2026-06-04T00:00:00.500000000Z keep-before-cursor",
		"2026-06-04T00:00:01.250000000Z keep-this",
		"2026-06-04T00:00:02.000000000Z drop-this",
	}, "\n")

	logs := dockerLogEntries(input, e2bapi.LogLevelError, SandboxLogsRequest{
		Cursor: &cursor,
		Search: &search,
	})

	if len(logs) != 1 {
		t.Fatalf("expected one filtered docker log, got %#v", logs)
	}
	if logs[0].Message != "keep-this" {
		t.Fatalf("expected timestamp prefix to be removed, got %q", logs[0].Message)
	}
	if logs[0].Timestamp.UnixMilli() != time.Date(2026, 6, 4, 0, 0, 1, 250000000, time.UTC).UnixMilli() {
		t.Fatalf("expected parsed docker timestamp, got %s", logs[0].Timestamp)
	}
	if logs[0].Level != e2bapi.LogLevelError || logs[0].Fields["source"] != "docker" {
		t.Fatalf("unexpected docker log metadata: %#v", logs[0])
	}
}

func TestDockerLogTailExpandsForFilteredQueries(t *testing.T) {
	limit := int32(25)
	search := "needle"
	if tail := dockerLogTail(SandboxLogsRequest{Limit: limit}); tail != 25 {
		t.Fatalf("expected unfiltered tail 25, got %d", tail)
	}
	if tail := dockerLogTail(SandboxLogsRequest{Limit: limit, Search: &search}); tail != 250 {
		t.Fatalf("expected filtered tail expansion, got %d", tail)
	}
	cursor := int64(1000)
	if tail := dockerLogTail(SandboxLogsRequest{Limit: limit, Cursor: &cursor}); tail != 0 {
		t.Fatalf("expected cursor query to disable docker tail, got %d", tail)
	}
}

func TestDockerSandboxLabelsRoundTripControlPlaneFields(t *testing.T) {
	createdAt := time.Date(2026, 6, 4, 1, 2, 3, 4, time.UTC)
	endAt := createdAt.Add(10 * time.Minute)
	allowInternet := false
	labels := dockerSandboxLabels(SandboxRuntimeCreateRequest{
		SandboxID:  "sbx_restore",
		TemplateID: "base",
		Metadata:   map[string]string{"source": "restore-test"},
		VolumeMounts: []VolumeMount{
			{Name: "data", Path: "/mnt/data"},
		},
		CreatedAt:           createdAt,
		EndAt:               endAt,
		AllowInternetAccess: &allowInternet,
	}, "base", "example/base:latest")

	if labels[dockerLocalSandboxIDLabel] != "sbx_restore" || labels[dockerLocalSandboxTemplateIDLabel] != "base" {
		t.Fatalf("missing sandbox identity labels: %#v", labels)
	}
	if dockerTimeLabel(labels[dockerLocalSandboxCreatedAtLabel], time.Time{}) != createdAt {
		t.Fatalf("expected created_at to round trip, got %q", labels[dockerLocalSandboxCreatedAtLabel])
	}
	if dockerTimeLabel(labels[dockerLocalSandboxEndAtLabel], time.Time{}) != endAt {
		t.Fatalf("expected end_at to round trip, got %q", labels[dockerLocalSandboxEndAtLabel])
	}
	if got := dockerStringMapLabel(labels[dockerLocalSandboxMetadataLabel]); got["source"] != "restore-test" {
		t.Fatalf("expected metadata to round trip, got %#v", got)
	}
	if got := dockerBoolPtrLabel(labels[dockerLocalSandboxAllowInternetLabel]); got == nil || *got {
		t.Fatalf("expected allow internet false, got %#v", got)
	}
	if got := dockerVolumeMountsFromLabels(labels); len(got) != 1 || got[0].Name != "data" || got[0].Path != "/mnt/data" {
		t.Fatalf("expected volume mounts to round trip, got %#v", got)
	}
}

func TestDockerfileFromTemplateBuildStartConvertsSupportedSteps(t *testing.T) {
	runtime := &DockerRuntime{cfg: gateway.DefaultConfig().Docker}
	runArgs := []string{"apt-get update"}
	envArgs := []string{"A", "1", "WITH_SPACE", "hello world"}
	workdirArgs := []string{"/app"}
	userArgs := []string{"user"}
	steps := []e2bapi.TemplateStep{
		{Type: "RUN", Args: &runArgs},
		{Type: "ENV", Args: &envArgs},
		{Type: "WORKDIR", Args: &workdirArgs},
		{Type: "USER", Args: &userArgs},
	}
	fromImage := "ubuntu:22.04"

	dockerfile, err := runtime.dockerfileFromTemplateBuildStart(context.Background(), e2bapi.TemplateBuildStartV2{
		FromImage: &fromImage,
		Steps:     &steps,
	})
	if err != nil {
		t.Fatalf("convert build start: %v", err)
	}

	want := strings.Join([]string{
		"FROM ubuntu:22.04",
		"RUN apt-get update",
		"ENV A=\"1\" WITH_SPACE=\"hello world\"",
		"WORKDIR /app",
		"USER user",
		"",
	}, "\n")
	if dockerfile != want {
		t.Fatalf("unexpected dockerfile:\n%s\nwant:\n%s", dockerfile, want)
	}
}

func TestDockerfileFromTemplateBuildStartConvertsCopyStep(t *testing.T) {
	runtime := &DockerRuntime{cfg: gateway.DefaultConfig().Docker}
	filesHash := "hash"
	copyArgs := []string{"package.json", "/app/package.json", "root:root", "0644"}
	steps := []e2bapi.TemplateStep{{Type: "COPY", Args: &copyArgs, FilesHash: &filesHash}}
	fromImage := "ubuntu:22.04"

	dockerfile, err := runtime.dockerfileFromTemplateBuildStart(context.Background(), e2bapi.TemplateBuildStartV2{
		FromImage: &fromImage,
		Steps:     &steps,
	})
	if err != nil {
		t.Fatalf("convert build start: %v", err)
	}

	want := "FROM ubuntu:22.04\nCOPY --chown=root:root --chmod=0644 package.json /app/package.json\n"
	if dockerfile != want {
		t.Fatalf("unexpected dockerfile:\n%s\nwant:\n%s", dockerfile, want)
	}
}

func TestDockerfileFromTemplateBuildStartRejectsCopyWithoutFilesHash(t *testing.T) {
	runtime := &DockerRuntime{cfg: gateway.DefaultConfig().Docker}
	copyArgs := []string{"package.json", "/app/package.json"}
	steps := []e2bapi.TemplateStep{{Type: "COPY", Args: &copyArgs}}
	fromImage := "ubuntu:22.04"

	_, err := runtime.dockerfileFromTemplateBuildStart(context.Background(), e2bapi.TemplateBuildStartV2{
		FromImage: &fromImage,
		Steps:     &steps,
	})
	if err == nil {
		t.Fatal("expected COPY step error")
	}
	if status := gatewayErrorStatus(err, 0); status != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %v", http.StatusBadRequest, status, err)
	}
}

func TestDockerSnapshotReferenceSanitizesName(t *testing.T) {
	name := "My Snapshot:/Default"
	ref := dockerSnapshotReference("sbx123", snapshotRequestName(e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody{Name: &name}), time.Unix(123, 0).UTC())

	if ref != "e2b-local/snapshots/my-snapshot-default:default" {
		t.Fatalf("unexpected snapshot reference %q", ref)
	}
}

func TestDockerSnapshotReferenceUsesSandboxFallback(t *testing.T) {
	ref := dockerSnapshotReference("sbx123", "", time.Unix(123, 0).UTC())

	if ref != "e2b-local/snapshots/sbx123-123:default" {
		t.Fatalf("unexpected fallback snapshot reference %q", ref)
	}
}

func TestSnapshotInfoFromDockerImagePrefersLabelReference(t *testing.T) {
	info := snapshotInfoFromDockerImage(image.Summary{
		ID:       "sha256:1234567890abcdef",
		RepoTags: []string{"e2b-local/snapshots/savepoint:default"},
		Labels: map[string]string{
			dockerLocalSnapshotRefLabel: "team/savepoint:default",
		},
	})

	if info.SnapshotID != "team/savepoint:default" {
		t.Fatalf("expected label snapshot id, got %#v", info)
	}
	if !reflect.DeepEqual(info.Names, []string{"e2b-local/snapshots/savepoint:default"}) {
		t.Fatalf("unexpected snapshot names: %#v", info.Names)
	}
}

func TestTemplateFromDockerImageRestoresGatewayLabels(t *testing.T) {
	template := templateFromDockerImage("fallback", "e2b-local/templates/custom:latest", image.Summary{
		ID:         "sha256:1234567890abcdef",
		RepoTags:   []string{"e2b-local/templates/custom:latest"},
		Containers: -1,
		Created:    1717200000,
		Size:       3 * 1024 * 1024,
		Labels: map[string]string{
			dockerLocalTemplateIDLabel:       "custom-template",
			dockerLocalTemplateNamesLabel:    "custom-template,custom-alias",
			dockerLocalTemplateBuildIDLabel:  "build-123",
			dockerLocalTemplateCPUCountLabel: "2",
			dockerLocalTemplateMemoryMBLabel: "1024",
		},
	})

	if template.TemplateID != "custom-template" || template.BuildID != "build-123" {
		t.Fatalf("unexpected template identity: %#v", template)
	}
	if !reflect.DeepEqual(template.Names, []string{"custom-template", "custom-alias"}) {
		t.Fatalf("unexpected template names: %#v", template.Names)
	}
	if template.CPUCount != 2 || template.MemoryMB != 1024 || template.DiskSizeMB != 3 {
		t.Fatalf("unexpected template resources: %#v", template)
	}
}

func gzipTarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		data := []byte(content)
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

type failingReader struct {
	data []byte
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("reader failed")
}

type gatedReader struct {
	data    []byte
	started chan struct{}
	release <-chan struct{}
	ready   bool
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if !r.ready {
		r.ready = true
		close(r.started)
		<-r.release
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
