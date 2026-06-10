package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMissingFileReturnsError(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("expected missing config file to return an error")
	}
}

func TestLoadConfigEmptyPathUsesDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}

	if cfg.Server.Addr != defaultServerAddr {
		t.Fatalf("expected server addr %q, got %q", defaultServerAddr, cfg.Server.Addr)
	}

	if cfg.Docker.Platform != "" {
		t.Fatalf("expected docker platform default to be empty, got %q", cfg.Docker.Platform)
	}

	if cfg.Docker.EnvdBinary != "" {
		t.Fatalf("expected docker envd binary default to be empty, got %q", cfg.Docker.EnvdBinary)
	}
}

func TestDefaultConfigUsesDockerHostEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/e2b-local-docker.sock")

	cfg := DefaultConfig()
	if cfg.Docker.Host != "unix:///tmp/e2b-local-docker.sock" {
		t.Fatalf("expected DOCKER_HOST default, got %q", cfg.Docker.Host)
	}
}

func TestLoadConfigReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  addr: "127.0.0.1:4000"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Server.Addr != "127.0.0.1:4000" {
		t.Fatalf("expected custom server addr, got %q", cfg.Server.Addr)
	}

}

func TestLoadConfigReadsDockerRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  addr: "127.0.0.1:4000"

runtime:
  type: "docker"

docker:
  host: "unix:///tmp/e2b-local-docker.sock"
  platform: "linux/amd64"
  container_name_prefix: "e2b-envd-"
  envd_binary: "/tmp/e2b-local-envd"
  health_timeout_seconds: 30
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Runtime.Type != "docker" {
		t.Fatalf("expected docker runtime, got %q", cfg.Runtime.Type)
	}

	if cfg.Docker.Host != "unix:///tmp/e2b-local-docker.sock" {
		t.Fatalf("expected OrbStack docker host, got %q", cfg.Docker.Host)
	}

	if cfg.Docker.Platform != "linux/amd64" {
		t.Fatalf("expected docker platform override, got %q", cfg.Docker.Platform)
	}
}

func TestLoadConfigResolvesRelativeLocalPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
runtime:
  type: "docker"

docker:
  host: "unix:///var/run/docker.sock"
  envd_binary: "envd-bin/envd-linux-amd64"

orbstack:
  envd_binary: "envd-bin/envd-linux-arm64"
  volume_host_path: "orbstack-volumes"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if want := filepath.Join(dir, "envd-bin", "envd-linux-amd64"); cfg.Docker.EnvdBinary != want {
		t.Fatalf("expected docker envd path %q, got %q", want, cfg.Docker.EnvdBinary)
	}
	if want := filepath.Join(dir, "envd-bin", "envd-linux-arm64"); cfg.Orbstack.EnvdBinary != want {
		t.Fatalf("expected orbstack envd path %q, got %q", want, cfg.Orbstack.EnvdBinary)
	}
	if want := filepath.Join(dir, "orbstack-volumes"); cfg.Orbstack.VolumeHostPath != want {
		t.Fatalf("expected orbstack volume path %q, got %q", want, cfg.Orbstack.VolumeHostPath)
	}
}

func TestLoadConfigReadsOrbstackRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
runtime:
  type: "orbstack"

orbstack:
  machine_name_prefix: "e2b-sandbox-"
  default_memory: "2G"
  default_cpus: "2"
  default_disk: "16G"
  isolated: true
  envd_binary: "/tmp/e2b-local-envd-arm64"
  envd_port: 49983
  health_timeout_seconds: 60
  volume_host_path: "/tmp/e2b-volumes"
  templates:
    ubuntu-2404:
      start_cmd: "python3 -m http.server 8080"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Runtime.Type != "orbstack" {
		t.Fatalf("expected orbstack runtime, got %q", cfg.Runtime.Type)
	}
	if cfg.Orbstack.MachineNamePrefix != "e2b-sandbox-" {
		t.Fatalf("expected machine prefix, got %q", cfg.Orbstack.MachineNamePrefix)
	}
	if !cfg.Orbstack.Isolated {
		t.Fatal("expected orbstack isolated mode enabled")
	}
	if cfg.Orbstack.VolumeHostPath != "/tmp/e2b-volumes" {
		t.Fatalf("expected volume host path, got %q", cfg.Orbstack.VolumeHostPath)
	}
	if len(cfg.Orbstack.Templates) != 1 {
		t.Fatalf("expected one orbstack template override, got %#v", cfg.Orbstack.Templates)
	}
	if cfg.Orbstack.Templates["ubuntu-2404"].BaseMachine != "" {
		t.Fatalf("expected template override base_machine to be optional, got %#v", cfg.Orbstack.Templates["ubuntu-2404"])
	}
	if cfg.Orbstack.Templates["ubuntu-2404"].StartCmd != "python3 -m http.server 8080" {
		t.Fatalf("expected template override start_cmd, got %#v", cfg.Orbstack.Templates["ubuntu-2404"])
	}
}

func TestPoolRuntimeTypeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
runtime:
  type: "pool"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected removed pool runtime type to fail")
	}
}

func TestStaticRuntimeTypeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
runtime:
  type: "static"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected removed static runtime type to fail")
	}
}

func TestUpstreamRuntimeTypeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
runtime:
  type: "upstream"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected removed upstream runtime type to fail")
	}
}

func TestE2BRuntimeTypeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
runtime:
  type: "e2b"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected removed e2b runtime type to fail")
	}
}
