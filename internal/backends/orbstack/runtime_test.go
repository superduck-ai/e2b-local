package orbstackbackend

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"
)

type fakeCloneCall struct {
	Source string
	Dest   string
}

type fakePathCall struct {
	Machine string
	Path    string
	Mode    fs.FileMode
}

type fakeWriteFileCall struct {
	Machine string
	Path    string
	Data    []byte
	Mode    fs.FileMode
}

type fakeSymlinkCall struct {
	Machine string
	Oldname string
	Newname string
}

type fakeShellCall struct {
	Machine string
	Script  string
}

type fakeMountCall struct {
	Machine string
	Source  string
	Dest    string
}

type fakeSetMachineOptionCall struct {
	Machine string
	Option  string
	Value   string
}

type fakeVMClient struct {
	deleteCalls           []string
	startCalls            []string
	stopCalls             []string
	cloneCalls            []fakeCloneCall
	mountCalls            []fakeMountCall
	setMachineOptionCalls []fakeSetMachineOptionCall
	mkdirCalls            []fakePathCall
	removeCalls           []fakePathCall
	writeFileCalls        []fakeWriteFileCall
	readFileCalls         []fakePathCall
	symlinkCalls          []fakeSymlinkCall
	shellCalls            []fakeShellCall
	infos                 map[string]VMInfo
	listVMs               []VMInfo
	readFiles             map[string][]byte
	events                []string
}

func (f *fakeVMClient) DeleteVM(ctx context.Context, name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	f.events = append(f.events, "delete:"+name)
	return nil
}

func (f *fakeVMClient) StartVM(ctx context.Context, name string) error {
	f.startCalls = append(f.startCalls, name)
	f.events = append(f.events, "start:"+name)
	return nil
}

func (f *fakeVMClient) StopVM(ctx context.Context, name string) error {
	f.stopCalls = append(f.stopCalls, name)
	f.events = append(f.events, "stop:"+name)
	return nil
}

func (f *fakeVMClient) GetVMInfo(ctx context.Context, name string) (VMInfo, error) {
	if info, ok := f.infos[name]; ok {
		return info, nil
	}
	for _, info := range f.infos {
		if info.ID == name {
			return info, nil
		}
	}
	return VMInfo{}, ErrVMNotFound
}

func (f *fakeVMClient) ListVMs(ctx context.Context) ([]VMInfo, error) {
	return append([]VMInfo(nil), f.listVMs...), nil
}

func (f *fakeVMClient) CloneVM(ctx context.Context, source, dest string) error {
	f.cloneCalls = append(f.cloneCalls, fakeCloneCall{Source: source, Dest: dest})
	f.events = append(f.events, "clone:"+dest)
	return nil
}

func (f *fakeVMClient) AddMachineMount(ctx context.Context, machine string, source string, dest string) error {
	f.mountCalls = append(f.mountCalls, fakeMountCall{
		Machine: machine,
		Source:  source,
		Dest:    dest,
	})
	f.events = append(f.events, "mount:"+machine+":"+dest)
	return nil
}

func (f *fakeVMClient) SetMachineOption(ctx context.Context, machine string, option string, value string) error {
	f.setMachineOptionCalls = append(f.setMachineOptionCalls, fakeSetMachineOptionCall{
		Machine: machine,
		Option:  option,
		Value:   value,
	})
	f.events = append(f.events, "config:"+machine+":"+option)
	return nil
}

func (f *fakeVMClient) MkdirAll(ctx context.Context, machine string, path string, mode fs.FileMode) error {
	f.mkdirCalls = append(f.mkdirCalls, fakePathCall{Machine: machine, Path: path, Mode: mode})
	f.events = append(f.events, "mkdir:"+machine+":"+path)
	return nil
}

func (f *fakeVMClient) RemoveAll(ctx context.Context, machine string, path string) error {
	f.removeCalls = append(f.removeCalls, fakePathCall{Machine: machine, Path: path})
	f.events = append(f.events, "remove:"+machine+":"+path)
	return nil
}

func (f *fakeVMClient) ReadFile(ctx context.Context, machine string, path string) ([]byte, error) {
	f.readFileCalls = append(f.readFileCalls, fakePathCall{Machine: machine, Path: path})
	f.events = append(f.events, "read:"+machine+":"+path)
	if f.readFiles != nil {
		if data, ok := f.readFiles[fakeFileKey(machine, path)]; ok {
			return append([]byte(nil), data...), nil
		}
	}
	return nil, os.ErrNotExist
}

func (f *fakeVMClient) WriteFile(ctx context.Context, machine string, path string, data []byte, mode fs.FileMode) error {
	f.writeFileCalls = append(f.writeFileCalls, fakeWriteFileCall{
		Machine: machine,
		Path:    path,
		Data:    append([]byte(nil), data...),
		Mode:    mode,
	})
	f.events = append(f.events, "write:"+machine+":"+path)
	return nil
}

func (f *fakeVMClient) Symlink(ctx context.Context, machine string, oldname string, newname string) error {
	f.symlinkCalls = append(f.symlinkCalls, fakeSymlinkCall{
		Machine: machine,
		Oldname: oldname,
		Newname: newname,
	})
	f.events = append(f.events, "symlink:"+machine+":"+newname)
	return nil
}

func (f *fakeVMClient) RunShell(ctx context.Context, machine string, script string) ([]byte, error) {
	f.shellCalls = append(f.shellCalls, fakeShellCall{
		Machine: machine,
		Script:  script,
	})
	f.events = append(f.events, "shell:"+machine)
	return nil, nil
}

func TestOrbstackRuntimeCreateSandboxClonesAndConfiguresMachine(t *testing.T) {
	now := time.Date(2026, 6, 8, 8, 0, 0, 0, time.UTC)
	envdPath := writeTestEnvdBinary(t)
	client := &fakeVMClient{
		infos: map[string]VMInfo{
			"ubuntu-2404": {
				ID:    "vm-base",
				Name:  "ubuntu-2404",
				State: "running",
				IP4:   "192.168.139.1",
			},
			"e2b-sandbox-sbx123": {
				ID:    "vm-123",
				Name:  "e2b-sandbox-sbx123",
				State: "running",
				IP4:   "192.168.139.10",
			},
		},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
			EnvdBinary:        envdPath,
			EnvdPort:          49983,
			Templates: map[string]OrbstackTemplateConfig{
				"ubuntu-2404": {
					StartCmd: "python3 -m http.server 8080",
				},
			},
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
		checkHealthy: func(ctx context.Context, envdURL string) error {
			if envdURL != "http://192.168.139.10:49983" {
				t.Fatalf("unexpected envd url %q", envdURL)
			}
			return nil
		},
		now:   func() time.Time { return now },
		newID: func() string { return "generated-id" },
	}

	info, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:  "sbx123",
		TemplateID: "ubuntu-2404",
		Metadata:   map[string]string{"source": "test"},
		EnvVars:    map[string]string{"FOO": "bar"},
		CreatedAt:  now,
		EndAt:      now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	if len(client.cloneCalls) != 1 || client.cloneCalls[0] != (fakeCloneCall{Source: "ubuntu-2404", Dest: "e2b-sandbox-sbx123"}) {
		t.Fatalf("unexpected clone calls: %#v", client.cloneCalls)
	}
	if len(client.startCalls) != 1 || client.startCalls[0] != "e2b-sandbox-sbx123" {
		t.Fatalf("unexpected start calls: %#v", client.startCalls)
	}
	envdWrite := assertWriteFile(t, client, "e2b-sandbox-sbx123", envdBinaryPath)
	if string(envdWrite.Data) != "envd-binary" || envdWrite.Mode != 0o755 {
		t.Fatalf("unexpected envd write: %#v", envdWrite)
	}
	serviceWrite := assertWriteFile(t, client, "e2b-sandbox-sbx123", envdServicePath)
	service := string(serviceWrite.Data)
	assertStringContains(t, service, `Environment="FOO=bar"`)
	assertStringContains(t, service, "ExecStart=/usr/local/bin/envd")
	metadataWrite := assertWriteFile(t, client, "e2b-sandbox-sbx123", sandboxMetadataPath)
	assertStringContains(t, string(metadataWrite.Data), `"template_id":"ubuntu-2404"`)
	assertSymlink(t, client, "e2b-sandbox-sbx123", "../envd.service", envdServiceWantsPath)
	assertShellContains(t, client, "e2b-sandbox-sbx123", "sudo systemctl restart envd")
	assertEventBefore(t, client.events, "start:e2b-sandbox-sbx123", "write:e2b-sandbox-sbx123:"+envdBinaryPath)
	if strings.Contains(service, "/mnt/mac") {
		t.Fatalf("expected service to avoid /mnt/mac for envd, got %q", service)
	}

	if info.ContainerID != "vm-123" || info.MachineID != "e2b-sandbox-sbx123" || info.ContainerIP != "192.168.139.10" {
		t.Fatalf("unexpected runtime info: %#v", info)
	}
	if info.EnvdURL != "http://192.168.139.10:49983" {
		t.Fatalf("unexpected envd url %q", info.EnvdURL)
	}
}

func TestOrbstackRuntimeCreateSandboxVolumeMountsForceSelectiveIsolatedMounts(t *testing.T) {
	now := time.Date(2026, 6, 8, 8, 0, 0, 0, time.UTC)
	hostPath := t.TempDir()
	envdPath := writeTestEnvdBinary(t)
	writeTestVolumeMetadata(t, OrbstackRuntimeConfig{VolumeHostPath: hostPath}, RuntimeVolume{
		VolumeID: "vol-1",
		Name:     "data",
	})
	writeTestVolumeMetadata(t, OrbstackRuntimeConfig{VolumeHostPath: hostPath}, RuntimeVolume{
		VolumeID: "vol-2",
		Name:     "cache",
	})
	client := &fakeVMClient{
		infos: map[string]VMInfo{
			"ubuntu-2404": {
				ID:    "vm-base",
				Name:  "ubuntu-2404",
				State: "running",
				IP4:   "192.168.139.1",
			},
			"e2b-sandbox-sbx123": {
				ID:    "vm-123",
				Name:  "e2b-sandbox-sbx123",
				State: "running",
				IP4:   "192.168.139.10",
			},
		},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
			VolumeHostPath:    hostPath,
			EnvdBinary:        envdPath,
			EnvdPort:          49983,
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
		checkHealthy: func(ctx context.Context, envdURL string) error {
			return nil
		},
		now: func() time.Time { return now },
	}

	_, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:  "sbx123",
		TemplateID: "ubuntu-2404",
		VolumeMounts: []gateway.VolumeMount{
			{VolumeID: "vol-1", Path: "/data"},
			{VolumeID: "vol-1", Path: "/data-again"},
			{VolumeID: "vol-2", Path: "/cache"},
		},
		CreatedAt: now,
		EndAt:     now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create sandbox with selective mounts: %v", err)
	}

	if len(client.setMachineOptionCalls) != 1 || client.setMachineOptionCalls[0] != (fakeSetMachineOptionCall{
		Machine: "e2b-sandbox-sbx123",
		Option:  "isolated",
		Value:   "true",
	}) {
		t.Fatalf("expected volume mounts to force isolated option, got %#v", client.setMachineOptionCalls)
	}
	if len(client.mountCalls) != 2 {
		t.Fatalf("expected two selective mount calls, got %#v", client.mountCalls)
	}
	dataDir := volumeBaseDir(runtime.cfg, volumeHostDirBaseName("data"))
	cacheDir := volumeBaseDir(runtime.cfg, volumeHostDirBaseName("cache"))
	if client.mountCalls[0] != (fakeMountCall{
		Machine: "e2b-sandbox-sbx123",
		Source:  dataDir,
		Dest:    isolatedVolumeSourcePath("vol-1"),
	}) {
		t.Fatalf("unexpected first mount call: %#v", client.mountCalls)
	}
	if client.mountCalls[1] != (fakeMountCall{
		Machine: "e2b-sandbox-sbx123",
		Source:  cacheDir,
		Dest:    isolatedVolumeSourcePath("vol-2"),
	}) {
		t.Fatalf("unexpected second mount call: %#v", client.mountCalls)
	}
	assertSymlink(t, client, "e2b-sandbox-sbx123", isolatedVolumeSourcePath("vol-1"), "/data")
	assertSymlink(t, client, "e2b-sandbox-sbx123", isolatedVolumeSourcePath("vol-1"), "/data-again")
	assertSymlink(t, client, "e2b-sandbox-sbx123", isolatedVolumeSourcePath("vol-2"), "/cache")
	for _, call := range client.symlinkCalls {
		if strings.Contains(call.Oldname, "/mnt/mac") || strings.Contains(call.Newname, "/mnt/mac") {
			t.Fatalf("expected configure symlinks to avoid /mnt/mac, got %#v", client.symlinkCalls)
		}
	}
}

func TestOrbstackRuntimeResolveVolumeMountsByVolumeName(t *testing.T) {
	hostPath := t.TempDir()
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			VolumeHostPath: hostPath,
		},
		vmClient: &fakeVMClient{},
		logger:   log.New(io.Discard, "", 0),
		now:      time.Now,
		newID:    func() string { return "unused" },
	}

	volume := RuntimeVolume{
		VolumeID: "vol-foo",
		Name:     "foo",
	}
	writeTestVolumeMetadata(t, runtime.cfg, volume)

	resolvedMounts, hostDirs, err := runtime.resolveVolumeMounts(context.Background(), []gateway.VolumeMount{
		{VolumeID: "foo", Path: "/mnt/data"},
	})
	if err != nil {
		t.Fatalf("resolve volume mounts: %v", err)
	}
	if len(resolvedMounts) != 1 {
		t.Fatalf("expected one resolved volume mount, got %#v", resolvedMounts)
	}
	if resolvedMounts[0].VolumeID != "vol-foo" || resolvedMounts[0].Name != "foo" || resolvedMounts[0].Path != "/mnt/data" {
		t.Fatalf("unexpected resolved volume mount: %#v", resolvedMounts[0])
	}
	if got := hostDirs["vol-foo"]; got == "" {
		t.Fatalf("expected host dir for real volume id, got %#v", hostDirs)
	}
}

func TestOrbstackRuntimeCreateSandboxIsolatedUsesOrbConfigAndDirectFileCopy(t *testing.T) {
	now := time.Date(2026, 6, 8, 8, 0, 0, 0, time.UTC)
	hostPath := t.TempDir()
	envdPath := writeTestEnvdBinary(t)
	writeTestVolumeMetadata(t, OrbstackRuntimeConfig{VolumeHostPath: hostPath}, RuntimeVolume{
		VolumeID: "vol-1",
		Name:     "data",
	})
	writeTestVolumeMetadata(t, OrbstackRuntimeConfig{VolumeHostPath: hostPath}, RuntimeVolume{
		VolumeID: "vol-2",
		Name:     "cache",
	})
	client := &fakeVMClient{
		infos: map[string]VMInfo{
			"ubuntu-2404": {
				ID:    "vm-base",
				Name:  "ubuntu-2404",
				State: "running",
				IP4:   "192.168.139.1",
			},
			"e2b-sandbox-sbx123": {
				ID:    "vm-123",
				Name:  "e2b-sandbox-sbx123",
				State: "running",
				IP4:   "192.168.139.10",
			},
		},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
			Isolated:          true,
			VolumeHostPath:    hostPath,
			EnvdBinary:        envdPath,
			EnvdPort:          49983,
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
		checkHealthy: func(ctx context.Context, envdURL string) error {
			return nil
		},
		now: func() time.Time { return now },
	}

	_, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:  "sbx123",
		TemplateID: "ubuntu-2404",
		VolumeMounts: []gateway.VolumeMount{
			{VolumeID: "vol-1", Path: "/data"},
			{VolumeID: "vol-1", Path: "/data-again"},
			{VolumeID: "vol-2", Path: "/cache"},
		},
		CreatedAt: now,
		EndAt:     now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create isolated sandbox: %v", err)
	}

	if len(client.setMachineOptionCalls) != 1 || client.setMachineOptionCalls[0] != (fakeSetMachineOptionCall{
		Machine: "e2b-sandbox-sbx123",
		Option:  "isolated",
		Value:   "true",
	}) {
		t.Fatalf("unexpected machine option calls: %#v", client.setMachineOptionCalls)
	}
	if len(client.mountCalls) != 2 {
		t.Fatalf("expected two selective mount calls, got %#v", client.mountCalls)
	}
	dataDir := volumeBaseDir(runtime.cfg, volumeHostDirBaseName("data"))
	cacheDir := volumeBaseDir(runtime.cfg, volumeHostDirBaseName("cache"))
	if client.mountCalls[0] != (fakeMountCall{
		Machine: "e2b-sandbox-sbx123",
		Source:  dataDir,
		Dest:    isolatedVolumeSourcePath("vol-1"),
	}) {
		t.Fatalf("unexpected first mount call: %#v", client.mountCalls)
	}
	if client.mountCalls[1] != (fakeMountCall{
		Machine: "e2b-sandbox-sbx123",
		Source:  cacheDir,
		Dest:    isolatedVolumeSourcePath("vol-2"),
	}) {
		t.Fatalf("unexpected second mount call: %#v", client.mountCalls)
	}
	envdWrite := assertWriteFile(t, client, "e2b-sandbox-sbx123", envdBinaryPath)
	if string(envdWrite.Data) != "envd-binary" || envdWrite.Mode != 0o755 {
		t.Fatalf("unexpected envd write: %#v", envdWrite)
	}
	assertSymlink(t, client, "e2b-sandbox-sbx123", isolatedVolumeSourcePath("vol-1"), "/data")
	assertSymlink(t, client, "e2b-sandbox-sbx123", isolatedVolumeSourcePath("vol-1"), "/data-again")
	assertSymlink(t, client, "e2b-sandbox-sbx123", isolatedVolumeSourcePath("vol-2"), "/cache")
	for _, call := range client.symlinkCalls {
		if strings.Contains(call.Oldname, "/mnt/mac") || strings.Contains(call.Newname, "/mnt/mac") {
			t.Fatalf("expected isolated configure symlinks to avoid /mnt/mac, got %#v", client.symlinkCalls)
		}
	}
}

func TestOrbstackRuntimeCreateSandboxRejectsMissingTemplateMachine(t *testing.T) {
	client := &fakeVMClient{
		infos: map[string]VMInfo{},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
			EnvdBinary:        "/tmp/e2b-local-envd-arm64",
			EnvdPort:          49983,
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
	}

	_, err := runtime.CreateSandbox(context.Background(), gateway.SandboxRuntimeCreateRequest{
		SandboxID:  "sbx123",
		TemplateID: "missing-base",
	})
	if err == nil {
		t.Fatal("expected create sandbox to fail when base machine is missing")
	}
	if !strings.Contains(err.Error(), "template missing-base not found") {
		t.Fatalf("expected template not found error, got %v", err)
	}
}

func TestOrbstackRuntimeRestoreSandboxesReadsMetadata(t *testing.T) {
	now := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
	metadata := sandboxMetadata{
		SandboxID:  "sbx-restored",
		TemplateID: "ubuntu-2404",
		Metadata:   map[string]string{"owner": "restore"},
		CreatedAt:  now,
		EndAt:      now.Add(5 * time.Minute),
		VolumeMounts: []VolumeMount{
			{VolumeID: "vol-1", Path: "/data"},
		},
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	client := &fakeVMClient{
		listVMs: []VMInfo{
			{Name: "other-machine", State: "running"},
			{Name: "e2b-sandbox-sbx-restored", ID: "vm-restored", State: "stopped"},
			{Name: "e2b-sandbox-snapshot-sbx-restored-foo", State: "stopped"},
		},
		infos: map[string]VMInfo{
			"e2b-sandbox-sbx-restored": {
				Name:  "e2b-sandbox-sbx-restored",
				ID:    "vm-restored",
				State: "stopped",
				IP4:   "192.168.139.88",
			},
			"e2b-sandbox-snapshot-sbx-restored-foo": {
				Name:  "e2b-sandbox-snapshot-sbx-restored-foo",
				State: "stopped",
			},
		},
		readFiles: map[string][]byte{
			fakeFileKey("e2b-sandbox-sbx-restored", sandboxMetadataPath): metadataJSON,
		},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
			EnvdPort:          49983,
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
		now:      func() time.Time { return now },
	}

	records, err := runtime.RestoreSandboxes(context.Background())
	if err != nil {
		t.Fatalf("restore sandboxes: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one restored record, got %#v", records)
	}

	record := records[0]
	if record.ID != "sbx-restored" || record.State != string(e2bapi.Paused) || record.RuntimeInfo.MachineID != "e2b-sandbox-sbx-restored" {
		t.Fatalf("unexpected restored record: %#v", record)
	}
	if record.TemplateID != "ubuntu-2404" {
		t.Fatalf("expected restored template id ubuntu-2404, got %#v", record)
	}
	if record.RuntimeInfo.ContainerIP != "192.168.139.88" || record.EnvdURL != "http://192.168.139.88:49983" {
		t.Fatalf("expected restored direct machine address, got %#v", record)
	}
	if len(record.RuntimeInfo.VolumeMounts) != 1 || record.RuntimeInfo.VolumeMounts[0].MountPath != "/data" {
		t.Fatalf("expected restored volume mounts, got %#v", record.RuntimeInfo.VolumeMounts)
	}
}

func TestOrbstackRuntimeListTemplatesUsesCurrentOrbMachines(t *testing.T) {
	client := &fakeVMClient{
		listVMs: []VMInfo{
			{Name: "ubuntu-2404"},
			{Name: "e2b-sandbox-sbx123"},
			{Name: "e2b-sandbox-snapshot-sbx123-first"},
		},
		infos: map[string]VMInfo{
			"ubuntu-2404": {
				ID:       "vm-base",
				Name:     "ubuntu-2404",
				State:    "stopped",
				DiskSize: 1000886272,
			},
			"e2b-sandbox-sbx123": {
				Name: "e2b-sandbox-sbx123",
			},
			"e2b-sandbox-snapshot-sbx123-first": {
				Name: "e2b-sandbox-snapshot-sbx123-first",
			},
		},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
			DefaultMemory:     "2G",
			DefaultCPUs:       "2",
			DefaultDisk:       "16G",
			Templates: map[string]OrbstackTemplateConfig{
				"ubuntu-2404": {
					Memory: "4G",
					CPUs:   "4",
				},
			},
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
	}

	templates, err := runtime.ListTemplates(context.Background())
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected one template, got %#v", templates)
	}

	template := templates[0]
	if template.TemplateID != "ubuntu-2404" || template.ImageRef != "ubuntu-2404" {
		t.Fatalf("unexpected template identity: %#v", template)
	}
	if template.BuildID != "vm-base" {
		t.Fatalf("expected build id vm-base, got %#v", template)
	}
	if template.CPUCount != 4 || template.MemoryMB != 4096 || template.DiskSizeMB != 955 {
		t.Fatalf("unexpected template resources: %#v", template)
	}
}

func TestOrbstackRuntimeVolumeLifecycleUsesHostFilesystem(t *testing.T) {
	hostPath := t.TempDir()
	idValues := []string{"vol-123"}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			VolumeHostPath: hostPath,
			DefaultMemory:  "2G",
			DefaultCPUs:    "2",
			DefaultDisk:    "16G",
		},
		vmClient: &fakeVMClient{},
		logger:   log.New(io.Discard, "", 0),
		now:      time.Now,
		newID: func() string {
			value := idValues[0]
			idValues = idValues[1:]
			return value
		},
	}

	created, err := runtime.CreateVolume(context.Background(), "data")
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if created.VolumeID != "vol-123" {
		t.Fatalf("unexpected created volume: %#v", created)
	}

	resolved, err := findVolumeByID(runtime.cfg, created.VolumeID)
	if err != nil {
		t.Fatalf("resolve created volume: %v", err)
	}
	if filepath.Base(resolved.HostDir) != "data" {
		t.Fatalf("expected readable volume dir name %q, got %q", "data", filepath.Base(resolved.HostDir))
	}

	metadataFile := volumeHostMetadataPath(resolved.HostDir)
	assertVolumeMetadataStorage(t, metadataFile)

	volumes, err := runtime.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(volumes) != 1 || volumes[0].VolumeID != "vol-123" {
		t.Fatalf("unexpected listed volumes: %#v", volumes)
	}

	volume, err := runtime.GetVolume(context.Background(), "vol-123")
	if err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if volume.Name != "data" {
		t.Fatalf("unexpected volume metadata: %#v", volume)
	}

	deleted, err := runtime.DeleteVolume(context.Background(), "vol-123")
	if err != nil {
		t.Fatalf("delete volume: %v", err)
	}
	if !deleted {
		t.Fatal("expected delete volume to report success")
	}

	if _, err := os.Stat(resolved.HostDir); !os.IsNotExist(err) {
		t.Fatalf("expected volume dir to be removed, got err=%v", err)
	}

	deletedAgain, err := runtime.DeleteVolume(context.Background(), "vol-123")
	if err != nil {
		t.Fatalf("delete missing volume: %v", err)
	}
	if deletedAgain {
		t.Fatal("expected second delete to report no-op")
	}
}

func TestOrbstackRuntimeGetVolumeMigratesLegacyMetadataFile(t *testing.T) {
	hostPath := t.TempDir()
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			VolumeHostPath: hostPath,
		},
		vmClient: &fakeVMClient{},
		logger:   log.New(io.Discard, "", 0),
		now:      time.Now,
		newID:    func() string { return "unused" },
	}

	legacyVolume := RuntimeVolume{
		VolumeID: "vol-legacy",
		Name:     "legacy-data",
	}
	writeLegacyTestVolumeMetadata(t, runtime.cfg, legacyVolume)

	volume, err := runtime.GetVolume(context.Background(), legacyVolume.VolumeID)
	if err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if volume.VolumeID != legacyVolume.VolumeID || volume.Name != legacyVolume.Name {
		t.Fatalf("unexpected migrated volume: %#v", volume)
	}

	resolved, err := findVolumeByID(runtime.cfg, legacyVolume.VolumeID)
	if err != nil {
		t.Fatalf("resolve migrated volume: %v", err)
	}
	if filepath.Base(resolved.HostDir) != "legacy-data" {
		t.Fatalf("expected migrated volume dir name %q, got %q", "legacy-data", filepath.Base(resolved.HostDir))
	}

	metadataFile := volumeHostMetadataPath(resolved.HostDir)
	assertVolumeMetadataStorage(t, metadataFile)

	listed, err := runtime.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("list volumes after migration: %v", err)
	}
	if len(listed) != 1 || listed[0].VolumeID != legacyVolume.VolumeID || listed[0].Name != legacyVolume.Name {
		t.Fatalf("unexpected volumes after migration: %#v", listed)
	}
}

func TestOrbstackRuntimeCreateVolumeUsesReadableDirsForDuplicateNames(t *testing.T) {
	hostPath := t.TempDir()
	idValues := []string{"vol-1", "vol-2"}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			VolumeHostPath: hostPath,
		},
		vmClient: &fakeVMClient{},
		logger:   log.New(io.Discard, "", 0),
		now:      time.Now,
		newID: func() string {
			value := idValues[0]
			idValues = idValues[1:]
			return value
		},
	}

	first, err := runtime.CreateVolume(context.Background(), "data")
	if err != nil {
		t.Fatalf("create first volume: %v", err)
	}
	second, err := runtime.CreateVolume(context.Background(), "data")
	if err != nil {
		t.Fatalf("create second volume: %v", err)
	}

	firstResolved, err := findVolumeByID(runtime.cfg, first.VolumeID)
	if err != nil {
		t.Fatalf("resolve first volume: %v", err)
	}
	secondResolved, err := findVolumeByID(runtime.cfg, second.VolumeID)
	if err != nil {
		t.Fatalf("resolve second volume: %v", err)
	}

	if filepath.Base(firstResolved.HostDir) != "data" {
		t.Fatalf("expected first readable dir %q, got %q", "data", filepath.Base(firstResolved.HostDir))
	}
	if filepath.Base(secondResolved.HostDir) != "data-2" {
		t.Fatalf("expected second readable dir %q, got %q", "data-2", filepath.Base(secondResolved.HostDir))
	}
}

func TestOrbstackRuntimeCreateSandboxSnapshotClonesMachine(t *testing.T) {
	now := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	client := &fakeVMClient{}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
		now:      func() time.Time { return now },
	}

	name := "Hello Snapshot"
	snapshot, err := runtime.CreateSandboxSnapshot(context.Background(), SandboxRecord{
		ID: "sbx123",
		RuntimeInfo: SandboxRuntimeInfo{
			MachineID: "e2b-sandbox-sbx123",
		},
	}, e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody{
		Name: &name,
	})
	if err != nil {
		t.Fatalf("create sandbox snapshot: %v", err)
	}

	expectedSnapshotID := "e2b-sandbox-snapshot-sbx123-hello-snapshot"
	if len(client.deleteCalls) != 1 || client.deleteCalls[0] != expectedSnapshotID {
		t.Fatalf("expected snapshot pre-delete call for %q, got %#v", expectedSnapshotID, client.deleteCalls)
	}
	if len(client.cloneCalls) != 1 || client.cloneCalls[0] != (fakeCloneCall{
		Source: "e2b-sandbox-sbx123",
		Dest:   expectedSnapshotID,
	}) {
		t.Fatalf("unexpected clone calls: %#v", client.cloneCalls)
	}
	if snapshot.SnapshotID != expectedSnapshotID || len(snapshot.Names) != 1 || snapshot.Names[0] != expectedSnapshotID {
		t.Fatalf("unexpected snapshot info: %#v", snapshot)
	}
}

func TestOrbstackRuntimeListSnapshotsFiltersAndPaginates(t *testing.T) {
	client := &fakeVMClient{
		listVMs: []VMInfo{
			{Name: "e2b-sandbox-sbx123"},
			{Name: "e2b-sandbox-snapshot-sbx123-b"},
			{Name: "e2b-sandbox-snapshot-sbx999-a"},
			{Name: "e2b-sandbox-snapshot-sbx123-a"},
		},
	}
	runtime := &OrbstackRuntime{
		cfg: OrbstackRuntimeConfig{
			MachineNamePrefix: "e2b-sandbox-",
		},
		vmClient: client,
		logger:   log.New(io.Discard, "", 0),
	}

	firstPage, err := runtime.ListSnapshots(context.Background(), SnapshotListRequest{
		SandboxID: "sbx123",
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("list snapshots first page: %v", err)
	}
	if len(firstPage) != 1 || firstPage[0].SnapshotID != "e2b-sandbox-snapshot-sbx123-a" {
		t.Fatalf("unexpected first snapshot page: %#v", firstPage)
	}

	secondPage, err := runtime.ListSnapshots(context.Background(), SnapshotListRequest{
		SandboxID: "sbx123",
		NextToken: firstPage[0].SnapshotID,
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("list snapshots second page: %v", err)
	}
	if len(secondPage) != 1 || secondPage[0].SnapshotID != "e2b-sandbox-snapshot-sbx123-b" {
		t.Fatalf("unexpected second snapshot page: %#v", secondPage)
	}
}

func assertStringContains(t *testing.T, value string, want string) {
	t.Helper()
	if !strings.Contains(value, want) {
		t.Fatalf("expected %q to contain %q", value, want)
	}
}

func assertWriteFile(t *testing.T, client *fakeVMClient, machine string, path string) fakeWriteFileCall {
	t.Helper()
	for _, call := range client.writeFileCalls {
		if call.Machine == machine && call.Path == path {
			return call
		}
	}
	t.Fatalf("expected write call machine=%s path=%s, got %#v", machine, path, client.writeFileCalls)
	return fakeWriteFileCall{}
}

func assertSymlink(t *testing.T, client *fakeVMClient, machine string, oldname string, newname string) {
	t.Helper()
	for _, call := range client.symlinkCalls {
		if call.Machine == machine && call.Oldname == oldname && call.Newname == newname {
			return
		}
	}
	t.Fatalf("expected symlink machine=%s %s -> %s, got %#v", machine, newname, oldname, client.symlinkCalls)
}

func assertShellContains(t *testing.T, client *fakeVMClient, machine string, want string) {
	t.Helper()
	for _, call := range client.shellCalls {
		if call.Machine == machine && strings.Contains(call.Script, want) {
			return
		}
	}
	t.Fatalf("expected shell call machine=%s containing %q, got %#v", machine, want, client.shellCalls)
}

func assertEventBefore(t *testing.T, events []string, before string, after string) {
	t.Helper()
	beforeIndex := -1
	afterIndex := -1
	for i, event := range events {
		if event == before && beforeIndex == -1 {
			beforeIndex = i
		}
		if event == after && afterIndex == -1 {
			afterIndex = i
		}
	}
	if beforeIndex == -1 || afterIndex == -1 || beforeIndex >= afterIndex {
		t.Fatalf("expected event %q before %q, got %#v", before, after, events)
	}
}

func fakeFileKey(machine string, path string) string {
	return machine + "\x00" + path
}

func writeTestEnvdBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "envd")
	if err := os.WriteFile(path, []byte("envd-binary"), 0o755); err != nil {
		t.Fatalf("write test envd binary: %v", err)
	}
	return path
}

func assertVolumeMetadataStorage(t *testing.T, metadataFile string) {
	t.Helper()

	_, err := os.Stat(metadataFile)
	if volumeMetadataStoredAsSidecarFile() {
		if err != nil {
			t.Fatalf("expected metadata file to exist: %v", err)
		}
		return
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected metadata file to be absent, got err=%v", err)
	}
}

func writeTestVolumeMetadata(t *testing.T, cfg OrbstackRuntimeConfig, volume RuntimeVolume) {
	t.Helper()

	dir, err := createVolumeHostDir(cfg, volume.Name)
	if err != nil {
		t.Fatalf("create test volume dir: %v", err)
	}
	if err := writeVolumeMetadata(dir, volume); err != nil {
		t.Fatalf("write volume metadata: %v", err)
	}
}

func writeLegacyTestVolumeMetadata(t *testing.T, cfg OrbstackRuntimeConfig, volume RuntimeVolume) {
	t.Helper()

	dir := volumeBaseDir(cfg, volume.VolumeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir legacy volume dir: %v", err)
	}
	data, err := json.Marshal(struct {
		VolumeID string `json:"VolumeID"`
		Name     string `json:"Name"`
		Token    string `json:"Token,omitempty"`
	}{
		VolumeID: volume.VolumeID,
		Name:     volume.Name,
		Token:    "legacy-token-" + volume.VolumeID,
	})
	if err != nil {
		t.Fatalf("marshal legacy volume metadata: %v", err)
	}
	if err := os.WriteFile(volumeHostMetadataPath(dir), data, 0o644); err != nil {
		t.Fatalf("write legacy volume metadata: %v", err)
	}
}
