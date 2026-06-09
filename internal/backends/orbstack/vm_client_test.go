package orbstackbackend

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
)

func TestVMClientCreateVMBuildsExpectedCommand(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)

	var gotBinary string
	var gotArgs []string
	client.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotBinary = name
		gotArgs = append([]string(nil), args...)
		return nil, nil
	}

	err := client.CreateVM(context.Background(), CreateVMRequest{
		Name:     "sandbox-1",
		Distro:   "ubuntu",
		Version:  "noble",
		Arch:     "arm64",
		Memory:   "4G",
		CPUs:     "2",
		Disk:     "64G",
		UserData: "/tmp/user-data.yaml",
	})
	if err != nil {
		t.Fatalf("create vm: %v", err)
	}

	if gotBinary != "/usr/local/bin/orb" {
		t.Fatalf("expected orb binary, got %q", gotBinary)
	}

	wantArgs := []string{
		"create",
		"--arch", "arm64",
		"--memory", "4G",
		"--cpus", "2",
		"--disk", "64G",
		"--user-data", "/tmp/user-data.yaml",
		"ubuntu:noble",
		"sandbox-1",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("unexpected create args\nwant: %#v\ngot:  %#v", wantArgs, gotArgs)
	}
}

func TestVMClientPushFileBuildsExpectedCommand(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)

	sourceFile := t.TempDir() + "/envd"
	if err := os.WriteFile(sourceFile, []byte("binary-data"), 0o600); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	var gotBinary string
	var gotArgs []string
	var gotStdin []byte
	client.streamRunner = func(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
		gotBinary = name
		gotArgs = append([]string(nil), args...)
		var err error
		gotStdin, err = io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}

	if err := client.PushFile(context.Background(), "sandbox-1", sourceFile, "/tmp/e2b-local-envd"); err != nil {
		t.Fatalf("push file: %v", err)
	}

	if gotBinary != "/usr/local/bin/orb" {
		t.Fatalf("expected orb binary, got %q", gotBinary)
	}

	wantArgs := []string{"run", "--machine", "sandbox-1", "/bin/sh", "-lc", "set -eu; mkdir -p '/tmp'; cat > '/tmp/e2b-local-envd'"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("unexpected push args\nwant: %#v\ngot:  %#v", wantArgs, gotArgs)
	}
	if string(gotStdin) != "binary-data" {
		t.Fatalf("unexpected streamed contents %q", gotStdin)
	}
}

func TestVMClientAddMachineMountBuildsExpectedCommand(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)

	var gotArgs []string
	client.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return nil, nil
	}

	if err := client.AddMachineMount(context.Background(), "sandbox-1", "/tmp/vol-1", "/mnt/e2b-local/volumes/vol-1"); err != nil {
		t.Fatalf("add machine mount: %v", err)
	}

	wantArgs := []string{"config", "add", "machine.sandbox-1.mounts", "/tmp/vol-1:/mnt/e2b-local/volumes/vol-1"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("unexpected config add args\nwant: %#v\ngot:  %#v", wantArgs, gotArgs)
	}
}

func TestVMClientSetMachineOptionBuildsExpectedCommand(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)

	var gotArgs []string
	client.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return nil, nil
	}

	if err := client.SetMachineOption(context.Background(), "sandbox-1", "isolated", "true"); err != nil {
		t.Fatalf("set machine option: %v", err)
	}

	wantArgs := []string{"config", "set", "machine.sandbox-1.isolated", "true"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("unexpected config args\nwant: %#v\ngot:  %#v", wantArgs, gotArgs)
	}
}

func TestVMClientListVMsDecodesJSON(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)
	client.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(`[
  {
    "id": "01KTK0Z32XA8Y4R8MVY2F4TZKN",
    "name": "ubuntu-2404",
    "image": {
      "distro": "ubuntu",
      "version": "noble",
      "arch": "arm64",
      "variant": "cloud"
    },
    "config": {
      "isolated": false,
      "forward_ssh_agent": true,
      "isolate_network": false,
      "default_username": "ubuntu",
      "http_port": 0,
      "https_port": 0
    },
    "builtin": false,
    "state": "running"
  }
]`), nil
	}

	vms, err := client.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("list vms: %v", err)
	}
	if len(vms) != 1 {
		t.Fatalf("expected one vm, got %#v", vms)
	}
	if vms[0].Name != "ubuntu-2404" || vms[0].Image.Distro != "ubuntu" || vms[0].State != "running" {
		t.Fatalf("unexpected vm info: %#v", vms[0])
	}
}

func TestVMClientGetVMInfoDecodesJSON(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)
	client.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte(`{
  "record": {
    "id": "01KTK0Z32XA8Y4R8MVY2F4TZKN",
    "name": "ubuntu-2404",
    "image": {
      "distro": "ubuntu",
      "version": "noble",
      "arch": "arm64",
      "variant": "cloud"
    },
    "config": {
      "isolated": false,
      "forward_ssh_agent": true,
      "isolate_network": false,
      "default_username": "ubuntu",
      "http_port": 0,
      "https_port": 0
    },
    "builtin": false,
    "state": "running"
  },
  "disk_size": 997879808,
  "ip4": "192.168.139.198",
  "ip6": "fd07:b51a:cc66:0:18cb:1bff:fe4a:2ea0"
}`), nil
	}

	info, err := client.GetVMInfo(context.Background(), "ubuntu-2404")
	if err != nil {
		t.Fatalf("get vm info: %v", err)
	}
	if info.ID != "01KTK0Z32XA8Y4R8MVY2F4TZKN" || info.IP4 != "192.168.139.198" || info.DiskSize != 997879808 {
		t.Fatalf("unexpected vm info: %#v", info)
	}
}

func TestVMClientGetVMInfoReturnsNotFound(t *testing.T) {
	client := NewVMClient("/usr/local/bin/orb", nil)
	client.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("machine sandbox-1 not found"), errors.New("exit status 1")
	}

	_, err := client.GetVMInfo(context.Background(), "sandbox-1")
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("expected ErrVMNotFound, got %T %[1]v", err)
	}
}
