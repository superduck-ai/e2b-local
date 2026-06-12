//go:build darwin && cgo

package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	gateway "e2b-local/internal/gateway"

	"github.com/docker/docker/errdefs"
)

type fakeAppleClient struct {
	image       ImageDescription
	volumes     map[string]VolumeConfig
	snapshots   []ContainerSnapshot
	createErr   error
	healthErr   error
	events      []string
	created     []ContainerConfiguration
	processes   []ProcessConfiguration
	deleted     []string
	stopErr     error
	deleteErr   error
	volumeErr   error
	networkErr  error
	inspectErrs map[string]error
}

func (f *fakeAppleClient) Ping(ctx context.Context) error {
	f.events = append(f.events, "ping")
	return nil
}

func (f *fakeAppleClient) ResolveImage(ctx context.Context, ref string) (ImageDescription, error) {
	f.events = append(f.events, "resolve-image:"+ref)
	return f.image, nil
}

func (f *fakeAppleClient) ContainerCreate(ctx context.Context, config ContainerConfiguration) error {
	f.events = append(f.events, "create:"+config.ID)
	f.created = append(f.created, config)
	return f.createErr
}

func (f *fakeAppleClient) ContainerBootstrap(ctx context.Context, id string) error {
	f.events = append(f.events, "bootstrap:"+id)
	return nil
}

func (f *fakeAppleClient) ContainerStartProcess(ctx context.Context, containerID, processID string) error {
	f.events = append(f.events, "start:"+containerID+":"+processID)
	return nil
}

func (f *fakeAppleClient) ContainerCopyIn(ctx context.Context, id, srcPath, dstPath string, mode uint32) error {
	f.events = append(f.events, "copy:"+id+":"+dstPath)
	return nil
}

func (f *fakeAppleClient) ContainerCreateProcess(ctx context.Context, containerID, processID string, config ProcessConfiguration) error {
	f.events = append(f.events, "process:"+containerID+":"+processID)
	f.processes = append(f.processes, config)
	return nil
}

func (f *fakeAppleClient) ContainerStop(ctx context.Context, id string) error {
	f.events = append(f.events, "stop:"+id)
	return f.stopErr
}

func (f *fakeAppleClient) ContainerDelete(ctx context.Context, id string, force bool) error {
	f.events = append(f.events, "delete:"+id)
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func (f *fakeAppleClient) ContainerList(ctx context.Context, filters ContainerListFilters) ([]ContainerSnapshot, error) {
	if len(filters.IDs) > 0 {
		f.events = append(f.events, "list-id:"+strings.Join(filters.IDs, ","))
		filtered := make([]ContainerSnapshot, 0, len(f.snapshots))
		for _, snapshot := range f.snapshots {
			for _, id := range filters.IDs {
				if snapshot.Configuration.ID == id {
					filtered = append(filtered, snapshot)
				}
			}
		}
		return filtered, nil
	}
	f.events = append(f.events, "list")
	return append([]ContainerSnapshot(nil), f.snapshots...), nil
}

func (f *fakeAppleClient) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	f.events = append(f.events, "volume-create:"+name)
	if f.volumes == nil {
		f.volumes = map[string]VolumeConfig{}
	}
	f.volumes[name] = VolumeConfig{Name: name, Driver: "local", Format: "ext4", Source: "/volumes/" + name, Labels: labels}
	return nil
}

func (f *fakeAppleClient) VolumeInspect(ctx context.Context, name string) (VolumeConfig, error) {
	f.events = append(f.events, "volume-inspect:"+name)
	if err := f.inspectErrs[name]; err != nil {
		return VolumeConfig{}, err
	}
	if volume, ok := f.volumes[name]; ok {
		return volume, nil
	}
	return VolumeConfig{}, errdefs.NotFound(errors.New("volume not found"))
}

func (f *fakeAppleClient) VolumeList(ctx context.Context) ([]VolumeConfig, error) {
	f.events = append(f.events, "volume-list")
	volumes := make([]VolumeConfig, 0, len(f.volumes))
	for _, volume := range f.volumes {
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

func (f *fakeAppleClient) VolumeDelete(ctx context.Context, name string) error {
	f.events = append(f.events, "volume-delete:"+name)
	if f.volumeErr != nil {
		return f.volumeErr
	}
	delete(f.volumes, name)
	return nil
}

func (f *fakeAppleClient) DefaultNetworkAttachment(ctx context.Context, containerID string) ([]AttachmentConfig, error) {
	f.events = append(f.events, "default-network:"+containerID)
	if f.networkErr != nil {
		return nil, f.networkErr
	}
	return []AttachmentConfig{{
		Network: "default",
		Options: AttachmentOptions{
			Hostname: containerID,
			MTU:      1280,
		},
	}}, nil
}

func TestAppleContainerRuntimeCreateSandboxBuildsConfigAndStartsEnvd(t *testing.T) {
	now := time.Date(2026, 6, 12, 8, 0, 0, 0, time.UTC)
	envdPath := writeTestAppleEnvdBinary(t)
	client := &fakeAppleClient{
		image: testImageDescription(),
		volumes: map[string]VolumeConfig{
			appleVolumeName("vol-1"): {
				Name:   appleVolumeName("vol-1"),
				Driver: "local",
				Format: "ext4",
				Source: "/var/lib/container/volumes/vol-1",
				Labels: map[string]string{
					appleLocalManagedLabel:    "true",
					appleLocalVolumeIDLabel:   "vol-1",
					appleLocalVolumeNameLabel: "data",
				},
			},
		},
	}
	runtime := newTestAppleRuntime(envdPath, client)
	runtime.findFreePort = func() (int, error) { return 55321, nil }
	runtime.checkHealthy = func(ctx context.Context, envdURL string) error {
		client.events = append(client.events, "health:"+envdURL)
		return nil
	}

	allowInternet := true
	info, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:           "sbx123",
		TemplateID:          "ubuntu-2404",
		Metadata:            map[string]string{"purpose": "test"},
		EnvVars:             map[string]string{"A": "1"},
		VolumeMounts:        []gateway.VolumeMount{{Name: "data", VolumeID: "vol-1", MountPath: "/data"}},
		CreatedAt:           now,
		EndAt:               now.Add(10 * time.Minute),
		AllowInternetAccess: &allowInternet,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	if info.EnvdURL != "http://127.0.0.1:55321" || info.HostPort != "55321" {
		t.Fatalf("unexpected runtime info: %#v", info)
	}
	if len(info.VolumeMounts) != 1 || info.VolumeMounts[0].Name != "data" || info.VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("unexpected volume mounts: %#v", info.VolumeMounts)
	}

	wantEvents := []string{
		"resolve-image:docker.io/library/ubuntu:24.04",
		"volume-inspect:e2b-vol-vol-1",
		"default-network:e2b-sandbox-sbx123",
		"create:e2b-sandbox-sbx123",
		"bootstrap:e2b-sandbox-sbx123",
		"start:e2b-sandbox-sbx123:e2b-sandbox-sbx123",
		"copy:e2b-sandbox-sbx123:/usr/local/bin/envd",
		"process:e2b-sandbox-sbx123:envd",
		"start:e2b-sandbox-sbx123:envd",
		"health:http://127.0.0.1:55321",
	}
	if !reflect.DeepEqual(client.events, wantEvents) {
		t.Fatalf("unexpected events:\nwant %#v\ngot  %#v", wantEvents, client.events)
	}

	if len(client.created) != 1 {
		t.Fatalf("expected one container config, got %d", len(client.created))
	}
	config := client.created[0]
	if config.Image.Descriptor.Digest != "sha256:abc" {
		t.Fatalf("expected resolved image descriptor, got %#v", config.Image)
	}
	if got := config.PublishedPorts[0]; got.HostPort != 55321 || got.ContainerPort != 49983 || got.HostAddress != "127.0.0.1" {
		t.Fatalf("unexpected published port: %#v", got)
	}
	if len(config.Mounts) != 1 {
		t.Fatalf("expected volume filesystem, got %#v", config.Mounts)
	}
	if !reflect.DeepEqual(config.InitProcess.Arguments, []string{appleInitSleepTime}) {
		t.Fatalf("expected portable init sleep args, got %#v", config.InitProcess.Arguments)
	}
	mountJSON, err := json.Marshal(config.Mounts[0])
	if err != nil {
		t.Fatalf("marshal mount: %v", err)
	}
	assertStringContains(t, string(mountJSON), `"volume":{"cache":{"on":{}},"format":"ext4","name":"e2b-vol-vol-1","sync":{"fsync":{}}}`)
	assertStringContains(t, string(mountJSON), `"destination":"/data"`)

	assertStringContains(t, config.Labels[appleLocalSandboxMetadataLabel], `"purpose":"test"`)
	assertStringContains(t, config.Labels[appleLocalSandboxVolumeMountsLabel], `"mountPath":"/data"`)
	assertStringContains(t, config.Labels[appleLocalSandboxEnvVarsLabel], `"A":"1"`)
	if config.Labels[appleLocalSandboxAllowInternetLabel] != "true" {
		t.Fatalf("expected allow-internet label, got %#v", config.Labels)
	}
	if len(config.Networks) != 1 || config.Networks[0].Network != "default" {
		t.Fatalf("expected default network attachment, got %#v", config.Networks)
	}

	if len(client.processes) != 1 {
		t.Fatalf("expected envd process config, got %d", len(client.processes))
	}
	process := client.processes[0]
	if !reflect.DeepEqual(process.Arguments, []string{"-isnotfc", "-port", "49983", "-cmd", "python main.py"}) {
		t.Fatalf("unexpected envd args: %#v", process.Arguments)
	}
	if !containsString(process.Environment, "A=1") || !containsString(process.Environment, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Fatalf("unexpected envd environment: %#v", process.Environment)
	}
}

func TestAppleContainerRuntimeCreateSandboxCleansUpAfterHealthFailure(t *testing.T) {
	envdPath := writeTestAppleEnvdBinary(t)
	client := &fakeAppleClient{image: testImageDescription()}
	runtime := newTestAppleRuntime(envdPath, client)
	runtime.findFreePort = func() (int, error) { return 55322, nil }
	runtime.checkHealthy = func(ctx context.Context, envdURL string) error {
		client.events = append(client.events, "health:"+envdURL)
		return errors.New("not healthy")
	}

	_, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:  "sbx123",
		TemplateID: "ubuntu-2404",
	})
	if err == nil {
		t.Fatal("expected create error")
	}
	if len(client.deleted) != 1 || client.deleted[0] != "e2b-sandbox-sbx123" {
		t.Fatalf("expected cleanup delete, got %#v", client.deleted)
	}
	assertEventBefore(t, client.events, "health:http://127.0.0.1:55322", "delete:e2b-sandbox-sbx123")
}

func TestAppleContainerRuntimeCreateSandboxPropagatesVolumeInspectError(t *testing.T) {
	envdPath := writeTestAppleEnvdBinary(t)
	inspectErr := &xpcProtocolError{Code: appleErrorCodeInternalError, Message: "volume service unavailable"}
	client := &fakeAppleClient{
		image: testImageDescription(),
		inspectErrs: map[string]error{
			appleVolumeName("vol-1"): inspectErr,
		},
	}
	runtime := newTestAppleRuntime(envdPath, client)

	_, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:    "sbx123",
		TemplateID:   "ubuntu-2404",
		VolumeMounts: []gateway.VolumeMount{{Name: "data", VolumeID: "vol-1", MountPath: "/data"}},
	})
	if err == nil {
		t.Fatal("expected volume inspect error")
	}
	assertStringContains(t, err.Error(), "inspect apple container volume e2b-vol-vol-1")
	if containsString(client.events, "volume-list") {
		t.Fatalf("non-not-found inspect errors must not fall back to volume list: %#v", client.events)
	}
	if len(client.created) != 0 {
		t.Fatalf("container must not be created after volume inspect failure: %#v", client.created)
	}
}

func TestAppleContainerRuntimeCreateSandboxDisablesNetworkWhenInternetIsFalse(t *testing.T) {
	envdPath := writeTestAppleEnvdBinary(t)
	client := &fakeAppleClient{image: testImageDescription()}
	runtime := newTestAppleRuntime(envdPath, client)
	runtime.checkHealthy = func(ctx context.Context, envdURL string) error { return nil }
	allowInternet := false

	_, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:           "sbx123",
		TemplateID:          "ubuntu-2404",
		AllowInternetAccess: &allowInternet,
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if len(client.created) != 1 {
		t.Fatalf("expected one container config, got %d", len(client.created))
	}
	if len(client.created[0].Networks) != 0 || client.created[0].DNS != nil {
		t.Fatalf("expected network and DNS disabled, got networks=%#v dns=%#v", client.created[0].Networks, client.created[0].DNS)
	}
	for _, event := range client.events {
		if strings.HasPrefix(event, "default-network:") {
			t.Fatalf("default network should not be resolved when internet is disabled: %#v", client.events)
		}
	}
}

func TestAppleContainerRuntimeDeleteAndPauseUseContainerLifecycle(t *testing.T) {
	client := &fakeAppleClient{}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)
	info := gateway.SandboxRuntimeInfo{ContainerID: "e2b-sandbox-sbx123"}

	if err := runtime.PauseSandbox(context.Background(), info); err != nil {
		t.Fatalf("PauseSandbox returned error: %v", err)
	}
	if err := runtime.DeleteSandbox(context.Background(), info); err != nil {
		t.Fatalf("DeleteSandbox returned error: %v", err)
	}
	want := []string{"stop:e2b-sandbox-sbx123", "delete:e2b-sandbox-sbx123"}
	if !reflect.DeepEqual(client.events, want) {
		t.Fatalf("unexpected lifecycle events: %#v", client.events)
	}
}

func TestAppleContainerRuntimeDeleteSandboxIgnoresNotFound(t *testing.T) {
	client := &fakeAppleClient{deleteErr: &xpcProtocolError{Code: appleErrorCodeNotFound, Message: "container with ID missing not found"}}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)

	if err := runtime.DeleteSandbox(context.Background(), gateway.SandboxRuntimeInfo{ContainerID: "missing"}); err != nil {
		t.Fatalf("DeleteSandbox should ignore not-found errors, got %v", err)
	}
}

func TestAppleContainerRuntimePauseSandboxIgnoresAlreadyStopped(t *testing.T) {
	client := &fakeAppleClient{stopErr: &xpcProtocolError{Code: appleErrorCodeInvalidState, Message: "container sbx is not running"}}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)

	if err := runtime.PauseSandbox(context.Background(), gateway.SandboxRuntimeInfo{ContainerID: "sbx"}); err != nil {
		t.Fatalf("PauseSandbox should ignore already-stopped errors, got %v", err)
	}
}

func TestAppleContainerRuntimeResumeReusesPersistedHostPortAndEnv(t *testing.T) {
	envdPath := writeTestAppleEnvdBinary(t)
	labels := map[string]string{
		appleLocalSandboxIDLabel:         "sbx-restored",
		appleLocalSandboxTemplateIDLabel: "ubuntu-2404",
		appleLocalSandboxEnvVarsLabel:    `{"TOKEN":"secret"}`,
	}
	client := &fakeAppleClient{
		image: testImageDescription(),
		snapshots: []ContainerSnapshot{{
			Status: "stopped",
			Configuration: ContainerConfiguration{
				ID:     "e2b-sandbox-sbx-restored",
				Labels: labels,
				PublishedPorts: []PublishPort{{
					HostAddress:   "127.0.0.1",
					HostPort:      56565,
					ContainerPort: 49983,
					Proto:         "tcp",
					Count:         1,
				}},
			},
		}},
	}
	runtime := newTestAppleRuntime(envdPath, client)
	runtime.findFreePort = func() (int, error) {
		t.Fatal("resume must not allocate a new host port")
		return 0, nil
	}
	runtime.checkHealthy = func(ctx context.Context, envdURL string) error {
		client.events = append(client.events, "health:"+envdURL)
		return nil
	}

	info, err := runtime.ResumeSandbox(context.Background(), gateway.SandboxRuntimeInfo{
		SandboxID:   "sbx-restored",
		ContainerID: "e2b-sandbox-sbx-restored",
	})
	if err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if info.HostPort != "56565" || info.EnvdURL != "http://127.0.0.1:56565" {
		t.Fatalf("unexpected resumed info: %#v", info)
	}
	if len(client.processes) != 1 || !containsString(client.processes[0].Environment, "TOKEN=secret") {
		t.Fatalf("expected persisted env vars in envd process, got %#v", client.processes)
	}
}

func TestAppleContainerRuntimeResumeRunningSandboxDoesNotRestartEnvd(t *testing.T) {
	client := &fakeAppleClient{
		snapshots: []ContainerSnapshot{{
			Status: "running",
			Configuration: ContainerConfiguration{
				ID: "e2b-sandbox-sbx-running",
				Labels: map[string]string{
					appleLocalSandboxIDLabel: "sbx-running",
				},
				PublishedPorts: []PublishPort{{
					HostAddress:   "127.0.0.1",
					HostPort:      56567,
					ContainerPort: 49983,
					Proto:         "tcp",
					Count:         1,
				}},
			},
		}},
	}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)
	runtime.checkHealthy = func(ctx context.Context, envdURL string) error {
		client.events = append(client.events, "health:"+envdURL)
		return nil
	}

	info, err := runtime.ResumeSandbox(context.Background(), gateway.SandboxRuntimeInfo{ContainerID: "e2b-sandbox-sbx-running"})
	if err != nil {
		t.Fatalf("ResumeSandbox returned error: %v", err)
	}
	if info.HostPort != "56567" || info.EnvdURL != "http://127.0.0.1:56567" {
		t.Fatalf("unexpected resumed info: %#v", info)
	}
	for _, event := range client.events {
		if strings.HasPrefix(event, "bootstrap:") || strings.HasPrefix(event, "start:") || strings.HasPrefix(event, "process:") || strings.HasPrefix(event, "copy:") {
			t.Fatalf("running resume must not restart envd, events=%#v", client.events)
		}
	}
}

func TestAppleContainerRuntimeListTemplatesUsesDefaultsAndReadyMetadata(t *testing.T) {
	listedAt := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), &fakeAppleClient{})
	runtime.now = func() time.Time { return listedAt }
	runtime.cfg.Templates = map[string]AppleContainerTemplateConfig{
		"debian-bookworm-slim": {
			Image: "docker.io/library/debian:bookworm-slim",
		},
		"ubuntu-2404": {
			Image:    "docker.io/library/ubuntu:24.04",
			CPUs:     2,
			MemoryMB: 2048,
		},
	}

	templates, err := runtime.ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListTemplates returned error: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("expected two templates, got %#v", templates)
	}

	first := templates[0]
	if first.TemplateID != "debian-bookworm-slim" || first.ImageRef != "docker.io/library/debian:bookworm-slim" {
		t.Fatalf("unexpected first template identity: %#v", first)
	}
	if !reflect.DeepEqual(first.Names, []string{"debian-bookworm-slim"}) {
		t.Fatalf("expected template names to include template ID, got %#v", first.Names)
	}
	if first.CPUCount != 4 || first.MemoryMB != 1024 {
		t.Fatalf("expected default resources, got cpu=%d memory=%d", first.CPUCount, first.MemoryMB)
	}
	if first.BuildCount != 1 || first.BuildID != "docker.io/library/debian:bookworm-slim" || first.BuildStatus != "ready" {
		t.Fatalf("expected ready build metadata, got %#v", first)
	}
	if !first.CreatedAt.Equal(listedAt) || !first.UpdatedAt.Equal(listedAt) {
		t.Fatalf("expected listed timestamps, got created=%s updated=%s", first.CreatedAt, first.UpdatedAt)
	}

	second := templates[1]
	if second.TemplateID != "ubuntu-2404" || second.CPUCount != 2 || second.MemoryMB != 2048 {
		t.Fatalf("expected explicit template resources, got %#v", second)
	}
}

func TestAppleContainerRuntimeRestoreSandboxesMapsMetadataAndPausedState(t *testing.T) {
	created := time.Date(2026, 6, 12, 8, 0, 0, 0, time.UTC)
	end := created.Add(5 * time.Minute)
	allow := false
	labels, err := appleSandboxLabels(gateway.SandboxRuntimeCreateRequest{
		SandboxID:           "sbx-restored",
		TemplateID:          "ubuntu-2404",
		Metadata:            map[string]string{"source": "restore"},
		VolumeMounts:        []gateway.VolumeMount{{Name: "data", VolumeID: "vol-1", MountPath: "/data"}},
		CreatedAt:           created,
		EndAt:               end,
		AllowInternetAccess: &allow,
	}, "ubuntu-2404", "docker.io/library/ubuntu:24.04", []gateway.VolumeMount{{Name: "data", VolumeID: "vol-1", MountPath: "/data"}})
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	client := &fakeAppleClient{
		snapshots: []ContainerSnapshot{{
			Status: "stopped",
			Configuration: ContainerConfiguration{
				ID:     "e2b-sandbox-sbx-restored",
				Labels: labels,
				PublishedPorts: []PublishPort{{
					HostAddress:   "127.0.0.1",
					HostPort:      56566,
					ContainerPort: 49983,
					Proto:         "tcp",
					Count:         1,
				}},
			},
		}},
	}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)

	records, err := runtime.RestoreSandboxes(context.Background())
	if err != nil {
		t.Fatalf("RestoreSandboxes returned error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one restored record, got %#v", records)
	}
	record := records[0]
	if record.State != "paused" || record.EnvdURL != "http://127.0.0.1:56566" || record.RuntimeInfo.HostPort != "56566" {
		t.Fatalf("unexpected restored record: %#v", record)
	}
	if record.Metadata["source"] != "restore" || !record.CreatedAt.Equal(created) || !record.EndAt.Equal(end) {
		t.Fatalf("expected metadata and dates to round-trip, got %#v", record)
	}
	if record.AllowInternetAccess == nil || *record.AllowInternetAccess {
		t.Fatalf("expected allow-internet=false, got %#v", record.AllowInternetAccess)
	}
	if len(record.RuntimeInfo.VolumeMounts) != 1 || record.RuntimeInfo.VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("expected volume mounts to restore, got %#v", record.RuntimeInfo.VolumeMounts)
	}
}

func TestAppleContainerRuntimeVolumeLifecycle(t *testing.T) {
	client := &fakeAppleClient{}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)
	runtime.newID = func() string { return "vol-123" }

	created, err := runtime.CreateVolume(context.Background(), "data")
	if err != nil {
		t.Fatalf("CreateVolume returned error: %v", err)
	}
	if created.VolumeID != "vol-123" || created.Name != "data" {
		t.Fatalf("unexpected created volume: %#v", created)
	}

	listed, err := runtime.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes returned error: %v", err)
	}
	if len(listed) != 1 || listed[0] != created {
		t.Fatalf("unexpected listed volumes: %#v", listed)
	}

	got, err := runtime.GetVolume(context.Background(), "vol-123")
	if err != nil {
		t.Fatalf("GetVolume returned error: %v", err)
	}
	if got != created {
		t.Fatalf("unexpected volume: %#v", got)
	}

	deleted, err := runtime.DeleteVolume(context.Background(), "vol-123")
	if err != nil {
		t.Fatalf("DeleteVolume returned error: %v", err)
	}
	if !deleted {
		t.Fatal("expected volume to be deleted")
	}
	deleted, err = runtime.DeleteVolume(context.Background(), "missing")
	if err != nil {
		t.Fatalf("DeleteVolume missing returned error: %v", err)
	}
	if deleted {
		t.Fatal("expected missing delete to return false")
	}
	_, err = runtime.GetVolume(context.Background(), "missing")
	if !errdefs.IsNotFound(err) {
		t.Fatalf("expected not-found get error, got %v", err)
	}
}

func TestAppleContainerRuntimeResolveVolumeMountFallsBackAfterAppleVolumeNotFound(t *testing.T) {
	client := &fakeAppleClient{
		volumes: map[string]VolumeConfig{
			appleVolumeName("vol-123"): {
				Name:   appleVolumeName("vol-123"),
				Driver: "local",
				Format: "ext4",
				Source: "/volumes/" + appleVolumeName("vol-123"),
				Labels: map[string]string{
					appleLocalManagedLabel:    "true",
					appleLocalVolumeIDLabel:   "vol-123",
					appleLocalVolumeNameLabel: "data",
				},
			},
		},
		inspectErrs: map[string]error{
			appleVolumeName("data"): &xpcProtocolError{
				Code:    appleErrorCodeInvalidArg,
				Message: "volume 'e2b-vol-data' not found",
			},
		},
	}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)

	volume, err := runtime.resolveVolumeForMount(context.Background(), gateway.VolumeMount{Name: "data", MountPath: "/data"})
	if err != nil {
		t.Fatalf("resolveVolumeForMount returned error: %v", err)
	}
	if volume.Name != appleVolumeName("vol-123") {
		t.Fatalf("expected fallback to managed volume list, got %#v", volume)
	}
	want := []string{"volume-inspect:e2b-vol-data", "volume-list"}
	if !reflect.DeepEqual(client.events, want) {
		t.Fatalf("unexpected events:\nwant %#v\ngot  %#v", want, client.events)
	}
}

func TestAppleContainerRuntimeDeleteVolumeIgnoresRaceWithRuntimeNotFound(t *testing.T) {
	client := &fakeAppleClient{
		volumes: map[string]VolumeConfig{
			appleVolumeName("vol-123"): {
				Name:   appleVolumeName("vol-123"),
				Driver: "local",
				Format: "ext4",
				Source: "/volumes/" + appleVolumeName("vol-123"),
				Labels: map[string]string{
					appleLocalManagedLabel:    "true",
					appleLocalVolumeIDLabel:   "vol-123",
					appleLocalVolumeNameLabel: "data",
				},
			},
		},
		volumeErr: &xpcProtocolError{Code: appleErrorCodeInvalidArg, Message: "volume 'e2b-vol-vol-123' not found"},
	}
	runtime := newTestAppleRuntime(writeTestAppleEnvdBinary(t), client)

	deleted, err := runtime.DeleteVolume(context.Background(), "vol-123")
	if err != nil {
		t.Fatalf("DeleteVolume should ignore runtime not-found races, got %v", err)
	}
	if deleted {
		t.Fatal("expected runtime not-found race to report deleted=false")
	}
}

func TestAppleDateUsesSwiftReferenceDateEncoding(t *testing.T) {
	date := AppleDate(time.Date(2001, 1, 1, 0, 0, 1, 500_000_000, time.UTC))
	data, err := json.Marshal(date)
	if err != nil {
		t.Fatalf("marshal AppleDate: %v", err)
	}
	if string(data) != "1.5" {
		t.Fatalf("expected Swift reference-date seconds, got %s", data)
	}
	var decoded AppleDate
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal AppleDate: %v", err)
	}
	if !time.Time(decoded).Equal(time.Time(date)) {
		t.Fatalf("expected date round-trip, got %s", time.Time(decoded))
	}
}

func TestAppleDateAcceptsNull(t *testing.T) {
	var decoded AppleDate
	if err := json.Unmarshal([]byte("null"), &decoded); err != nil {
		t.Fatalf("unmarshal null AppleDate: %v", err)
	}
	if !time.Time(decoded).IsZero() {
		t.Fatalf("expected null AppleDate to leave zero value, got %s", time.Time(decoded))
	}
}

func newTestAppleRuntime(envdPath string, client *fakeAppleClient) *AppleContainerRuntime {
	return &AppleContainerRuntime{
		cfg: AppleContainerRuntimeConfig{
			ContainerNamePrefix:  "e2b-sandbox-",
			EnvdBinary:           envdPath,
			EnvdPort:             49983,
			HealthTimeoutSeconds: 1,
			DefaultCPUs:          4,
			DefaultMemoryMB:      1024,
			Templates: map[string]AppleContainerTemplateConfig{
				"ubuntu-2404": {
					Image:    "docker.io/library/ubuntu:24.04",
					CPUs:     2,
					MemoryMB: 512,
					StartCmd: "python main.py",
				},
			},
		},
		client:     client,
		logger:     log.New(io.Discard, "", 0),
		httpClient: &http.Client{},
		findFreePort: func() (int, error) {
			return 55321, nil
		},
		now:   time.Now,
		newID: func() string { return "id-1" },
	}
}

func testImageDescription() ImageDescription {
	return ImageDescription{
		Reference: "docker.io/library/ubuntu:24.04",
		Descriptor: Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    "sha256:abc",
			Size:      123,
		},
	}
}

func writeTestAppleEnvdBinary(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/envd"
	if err := os.WriteFile(path, []byte("envd-binary"), 0o755); err != nil {
		t.Fatalf("write test envd binary: %v", err)
	}
	return path
}

func assertStringContains(t *testing.T, value string, want string) {
	t.Helper()
	if !strings.Contains(value, want) {
		t.Fatalf("expected %q to contain %q", value, want)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertEventBefore(t *testing.T, events []string, before string, after string) {
	t.Helper()
	beforeIndex := -1
	afterIndex := -1
	for i, event := range events {
		if event == before {
			beforeIndex = i
		}
		if event == after {
			afterIndex = i
		}
	}
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf("expected event %q before %q in %#v", before, after, events)
	}
}
