package orbstackbackend

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"e2b-local/internal/orbctl"
)

type fakeOrbControl struct {
	deleteCalls  []string
	startCalls   []string
	stopCalls    []string
	cloneCalls   []fakeCloneCall
	mountCalls   []fakeMountCall
	isolateCalls []fakeSetMachineOptionCall
	infos        map[string]orbctl.ContainerInfo
	machines     []orbctl.ContainerRecord
	err          error
}

type fakeMachineSSHCall struct {
	Machine string
	Script  string
	Stdin   []byte
}

func (f *fakeOrbControl) Delete(ctx context.Context, name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	return f.err
}

func (f *fakeOrbControl) Start(ctx context.Context, name string) error {
	f.startCalls = append(f.startCalls, name)
	return f.err
}

func (f *fakeOrbControl) Stop(ctx context.Context, name string) error {
	f.stopCalls = append(f.stopCalls, name)
	return f.err
}

func (f *fakeOrbControl) Info(ctx context.Context, name string) (orbctl.ContainerInfo, error) {
	if f.err != nil {
		return orbctl.ContainerInfo{}, f.err
	}
	if info, ok := f.infos[name]; ok {
		return info, nil
	}
	return orbctl.ContainerInfo{}, orbctl.ErrContainerNotFound
}

func (f *fakeOrbControl) ListMachines(ctx context.Context) ([]orbctl.ContainerRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]orbctl.ContainerRecord(nil), f.machines...), nil
}

func (f *fakeOrbControl) Clone(ctx context.Context, source string, dest string) error {
	f.cloneCalls = append(f.cloneCalls, fakeCloneCall{Source: source, Dest: dest})
	return f.err
}

func (f *fakeOrbControl) AddMount(ctx context.Context, name string, source string, dest string) error {
	f.mountCalls = append(f.mountCalls, fakeMountCall{Machine: name, Source: source, Dest: dest})
	return f.err
}

func (f *fakeOrbControl) SetIsolated(ctx context.Context, name string, isolated bool) error {
	f.isolateCalls = append(f.isolateCalls, fakeSetMachineOptionCall{
		Machine: name,
		Option:  "isolated",
		Value:   strconvBool(isolated),
	})
	return f.err
}

func TestVMClientLifecycleUsesOrbControl(t *testing.T) {
	orb := &fakeOrbControl{}
	client := &VMClient{orb: orb, orbRoot: t.TempDir()}

	if err := client.CloneVM(context.Background(), "ubuntu-2404", "sandbox-1"); err != nil {
		t.Fatalf("clone vm: %v", err)
	}
	if err := client.StartVM(context.Background(), "sandbox-1"); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	if err := client.StopVM(context.Background(), "sandbox-1"); err != nil {
		t.Fatalf("stop vm: %v", err)
	}
	if err := client.DeleteVM(context.Background(), "sandbox-1"); err != nil {
		t.Fatalf("delete vm: %v", err)
	}

	if len(orb.cloneCalls) != 1 || orb.cloneCalls[0] != (fakeCloneCall{Source: "ubuntu-2404", Dest: "sandbox-1"}) {
		t.Fatalf("unexpected clone calls: %#v", orb.cloneCalls)
	}
	if len(orb.startCalls) != 1 || orb.startCalls[0] != "sandbox-1" {
		t.Fatalf("unexpected start calls: %#v", orb.startCalls)
	}
	if len(orb.stopCalls) != 1 || orb.stopCalls[0] != "sandbox-1" {
		t.Fatalf("unexpected stop calls: %#v", orb.stopCalls)
	}
	if len(orb.deleteCalls) != 1 || orb.deleteCalls[0] != "sandbox-1" {
		t.Fatalf("unexpected delete calls: %#v", orb.deleteCalls)
	}
}

func TestVMClientConfigUsesOrbControl(t *testing.T) {
	orb := &fakeOrbControl{}
	client := &VMClient{orb: orb, orbRoot: t.TempDir()}

	if err := client.AddMachineMount(context.Background(), "sandbox-1", "/tmp/vol-1", "/mnt/e2b-local/volumes/vol-1"); err != nil {
		t.Fatalf("add machine mount: %v", err)
	}
	if err := client.SetMachineOption(context.Background(), "sandbox-1", "isolated", "true"); err != nil {
		t.Fatalf("set machine option: %v", err)
	}

	if len(orb.mountCalls) != 1 || orb.mountCalls[0] != (fakeMountCall{
		Machine: "sandbox-1",
		Source:  "/tmp/vol-1",
		Dest:    "/mnt/e2b-local/volumes/vol-1",
	}) {
		t.Fatalf("unexpected mount calls: %#v", orb.mountCalls)
	}
	if len(orb.isolateCalls) != 1 || orb.isolateCalls[0] != (fakeSetMachineOptionCall{
		Machine: "sandbox-1",
		Option:  "isolated",
		Value:   "true",
	}) {
		t.Fatalf("unexpected isolate calls: %#v", orb.isolateCalls)
	}
}

func TestVMClientListAndInfoMapOrbRecords(t *testing.T) {
	diskSize := int64(997879808)
	record := orbctl.ContainerRecord{
		ID:    "01KTK0Z32XA8Y4R8MVY2F4TZKN",
		Name:  "ubuntu-2404",
		State: "running",
		Image: orbctl.ContainerImage{
			Distro:  "ubuntu",
			Version: "noble",
			Arch:    "arm64",
			Variant: "cloud",
		},
		Config: orbctl.ContainerConfig{
			Isolated:        true,
			ForwardSSHAgent: true,
			DefaultUsername: "ubuntu",
		},
	}
	orb := &fakeOrbControl{
		infos: map[string]orbctl.ContainerInfo{
			"ubuntu-2404": {
				Record:   record,
				DiskSize: &diskSize,
				IP4:      "192.168.139.198",
				IP6:      "fd07:b51a:cc66:0:18cb:1bff:fe4a:2ea0",
			},
		},
		machines: []orbctl.ContainerRecord{record},
	}
	client := &VMClient{orb: orb, orbRoot: t.TempDir()}

	vms, err := client.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("list vms: %v", err)
	}
	if len(vms) != 1 || vms[0].Name != "ubuntu-2404" || !vms[0].Config.Isolated {
		t.Fatalf("unexpected vms: %#v", vms)
	}

	info, err := client.GetVMInfo(context.Background(), "ubuntu-2404")
	if err != nil {
		t.Fatalf("get vm info: %v", err)
	}
	if info.ID != record.ID || info.IP4 != "192.168.139.198" || info.DiskSize != diskSize {
		t.Fatalf("unexpected vm info: %#v", info)
	}
}

func TestVMClientGetVMInfoReturnsNotFound(t *testing.T) {
	client := &VMClient{
		orb: &fakeOrbControl{
			infos: map[string]orbctl.ContainerInfo{},
		},
		orbRoot: t.TempDir(),
	}

	_, err := client.GetVMInfo(context.Background(), "sandbox-1")
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("expected ErrVMNotFound, got %T %[1]v", err)
	}
}

func TestVMClientWritesAndLinksMachineFilesThroughSSHSocket(t *testing.T) {
	root := t.TempDir()
	var calls []fakeMachineSSHCall
	client := &VMClient{
		orb:     &fakeOrbControl{},
		orbRoot: root,
		sshRunner: func(ctx context.Context, machine string, stdin io.Reader, script string) ([]byte, error) {
			var data []byte
			if stdin != nil {
				var err error
				data, err = io.ReadAll(stdin)
				if err != nil {
					return nil, err
				}
			}
			calls = append(calls, fakeMachineSSHCall{
				Machine: machine,
				Script:  script,
				Stdin:   data,
			})
			return nil, nil
		},
	}

	if err := client.MkdirAll(context.Background(), "sandbox-1", "/usr/local/bin", 0o755); err != nil {
		t.Fatalf("mkdir file parent: %v", err)
	}
	if err := client.WriteFile(context.Background(), "sandbox-1", "/usr/local/bin/envd", []byte("binary-data"), 0o755); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := client.Symlink(context.Background(), "sandbox-1", "../envd.service", "/etc/systemd/system/multi-user.target.wants/envd.service"); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := client.RemoveAll(context.Background(), "sandbox-1", "/tmp/e2b-local"); err != nil {
		t.Fatalf("remove path: %v", err)
	}

	if len(calls) != 4 {
		t.Fatalf("expected four ssh calls, got %#v", calls)
	}
	if calls[0].Machine != "sandbox-1" || !strings.Contains(calls[0].Script, "sudo mkdir -p -- '/usr/local/bin'") {
		t.Fatalf("unexpected mkdir ssh call: %#v", calls[0])
	}
	if calls[1].Machine != "sandbox-1" || string(calls[1].Stdin) != "binary-data" {
		t.Fatalf("unexpected write ssh call: %#v", calls[1])
	}
	assertStringContains(t, calls[1].Script, "sudo install -m 0755")
	assertStringContains(t, calls[1].Script, "'/usr/local/bin/envd'")
	assertStringContains(t, calls[2].Script, "sudo ln -s -- '../envd.service' '/etc/systemd/system/multi-user.target.wants/envd.service'")
	assertStringContains(t, calls[3].Script, "sudo rm -rf -- '/tmp/e2b-local'")
}

func TestVMClientReadFileUsesOrbStackRootThenSSHFallback(t *testing.T) {
	root := t.TempDir()
	client := &VMClient{orb: &fakeOrbControl{}, orbRoot: root}
	hostFile := filepath.Join(root, "sandbox-1", "var", "lib", "e2b-local", "sandbox.json")
	if err := os.MkdirAll(filepath.Dir(hostFile), 0o755); err != nil {
		t.Fatalf("mkdir host path: %v", err)
	}
	if err := os.WriteFile(hostFile, []byte("metadata"), 0o644); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	got, err := client.ReadFile(context.Background(), "sandbox-1", "/var/lib/e2b-local/sandbox.json")
	if err != nil {
		t.Fatalf("read vm file: %v", err)
	}
	if string(got) != "metadata" {
		t.Fatalf("unexpected read data %q", got)
	}

	var fallbackCalls []fakeMachineSSHCall
	client.sshRunner = func(ctx context.Context, machine string, stdin io.Reader, script string) ([]byte, error) {
		fallbackCalls = append(fallbackCalls, fakeMachineSSHCall{
			Machine: machine,
			Script:  script,
		})
		return []byte("remote-data"), nil
	}
	got, err = client.ReadFile(context.Background(), "sandbox-1", "/missing.txt")
	if err != nil {
		t.Fatalf("read vm file via ssh fallback: %v", err)
	}
	if string(got) != "remote-data" {
		t.Fatalf("unexpected fallback data %q", got)
	}
	if len(fallbackCalls) != 1 || !strings.Contains(fallbackCalls[0].Script, "sudo cat -- '/missing.txt'") {
		t.Fatalf("unexpected fallback calls: %#v", fallbackCalls)
	}
}

func TestVMClientRejectsRemovingMachineRoot(t *testing.T) {
	client := &VMClient{orb: &fakeOrbControl{}, orbRoot: t.TempDir()}
	if err := client.RemoveAll(context.Background(), "sandbox-1", "/"); err == nil {
		t.Fatal("expected remove root to fail")
	}
}

func strconvBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
