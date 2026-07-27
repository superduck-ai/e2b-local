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
	if !cfg.Docker.EnableFUSE {
		t.Fatal("expected docker FUSE support to be enabled by default")
	}
	if cfg.Docker.VolumeHostPath == "" || !filepath.IsAbs(cfg.Docker.VolumeHostPath) {
		t.Fatalf("expected docker volume host path default to be absolute, got %q", cfg.Docker.VolumeHostPath)
	}
	if cfg.TemplateBuilds.MaxConcurrent != defaultTemplateBuildMaxConcurrent {
		t.Fatalf("expected template build concurrency default %d, got %d", defaultTemplateBuildMaxConcurrent, cfg.TemplateBuilds.MaxConcurrent)
	}

	if cfg.Traffic.AdvertisedProbeAddr != defaultTrafficProbeAddr {
		t.Fatalf("expected traffic probe addr %q, got %q", defaultTrafficProbeAddr, cfg.Traffic.AdvertisedProbeAddr)
	}
	if cfg.Traffic.Interface != "" {
		t.Fatalf("expected traffic interface default to be empty, got %q", cfg.Traffic.Interface)
	}

	if cfg.Docker.PublishedHostIP != defaultDockerPublishedHostIP {
		t.Fatalf("expected docker published host IP %q, got %q", defaultDockerPublishedHostIP, cfg.Docker.PublishedHostIP)
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

traffic:
  advertised_host: "192.0.2.10"
  interface: "en0"
  advertised_probe_addr: "1.1.1.1:53"

runtime:
  type: "docker"

docker:
  host: "unix:///tmp/e2b-local-docker.sock"
  platform: "linux/amd64"
  container_name_prefix: "e2b-envd-"
  envd_binary: "/tmp/e2b-local-envd"
  enable_fuse: false
  published_ports: [5000, 5001]
  published_host_ip: "0.0.0.0"
  health_timeout_seconds: 30
  volume_host_path: "/tmp/e2b-docker-volumes"
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
	if cfg.Docker.EnableFUSE {
		t.Fatal("expected docker FUSE support to be disabled by explicit config")
	}
	if cfg.Traffic.AdvertisedHost != "192.0.2.10" {
		t.Fatalf("expected traffic advertised host, got %q", cfg.Traffic.AdvertisedHost)
	}
	if cfg.Traffic.Interface != "en0" {
		t.Fatalf("expected traffic interface, got %q", cfg.Traffic.Interface)
	}
	if cfg.Traffic.AdvertisedProbeAddr != "1.1.1.1:53" {
		t.Fatalf("expected traffic probe addr, got %q", cfg.Traffic.AdvertisedProbeAddr)
	}
	if len(cfg.Docker.PublishedPorts) != 2 || cfg.Docker.PublishedPorts[0] != 5000 || cfg.Docker.PublishedPorts[1] != 5001 {
		t.Fatalf("expected docker published ports, got %#v", cfg.Docker.PublishedPorts)
	}
	if cfg.Docker.PublishedHostIP != "0.0.0.0" {
		t.Fatalf("expected docker published host IP, got %q", cfg.Docker.PublishedHostIP)
	}
	if cfg.Docker.VolumeHostPath != "/tmp/e2b-docker-volumes" {
		t.Fatalf("expected docker volume host path, got %q", cfg.Docker.VolumeHostPath)
	}
}

func TestLoadConfigRejectsInvalidTrafficAdvertisedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
traffic:
  advertised_host: "http://192.0.2.10"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected advertised_host with scheme to fail")
	}
}

func TestLoadConfigRejectsInvalidTrafficInterface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
traffic:
  interface: "en 0"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected interface with whitespace to fail")
	}
}

func TestResolveTrafficAdvertisedHostRejectsMissingConfiguredInterface(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Traffic.Interface = "e2b-local-missing0"

	if _, err := cfg.ResolveTrafficAdvertisedHost(); err == nil {
		t.Fatal("expected missing configured interface to fail")
	}
}

func TestLoadConfigRejectsInvalidDockerPublishedPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
docker:
  published_ports: [0]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected invalid docker published port to fail")
	}
}

func TestDockerRuntimeConfigValidateRequiresAbsoluteVolumeHostPath(t *testing.T) {
	cfg := DefaultConfig().Docker
	cfg.VolumeHostPath = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing docker volume host path to fail")
	}

	cfg = DefaultConfig().Docker
	cfg.VolumeHostPath = "relative-volumes"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected relative docker volume host path to fail")
	}
}

func TestLoadConfigReadsTemplateBuildLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
template_builds:
  max_concurrent: 4
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.TemplateBuilds.MaxConcurrent != 4 {
		t.Fatalf("expected template build concurrency override, got %d", cfg.TemplateBuilds.MaxConcurrent)
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
  volume_host_path: "docker-volumes"

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
	if want := filepath.Join(dir, "docker-volumes"); cfg.Docker.VolumeHostPath != want {
		t.Fatalf("expected docker volume path %q, got %q", want, cfg.Docker.VolumeHostPath)
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

func TestLoadConfigReadsAppleContainerRuntime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
runtime:
  type: "applecontainer"

applecontainer:
  container_name_prefix: "e2b-sandbox-"
  envd_binary: "envd-bin/envd-linux-arm64"
  envd_port: 49983
  health_timeout_seconds: 60
  default_cpus: 4
  default_memory_mb: 2048
  templates:
    ubuntu-2404:
      image: "docker.io/library/ubuntu:24.04"
      cpus: 2
      memory_mb: 1024
      start_cmd: "sleep infinity"
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Runtime.Type != "applecontainer" {
		t.Fatalf("expected applecontainer runtime, got %q", cfg.Runtime.Type)
	}
	if cfg.AppleContainer.ContainerNamePrefix != "e2b-sandbox-" {
		t.Fatalf("expected applecontainer container name prefix %q, got %q", "e2b-sandbox-", cfg.AppleContainer.ContainerNamePrefix)
	}
	if want := filepath.Join(dir, "envd-bin", "envd-linux-arm64"); cfg.AppleContainer.EnvdBinary != want {
		t.Fatalf("expected applecontainer envd path %q, got %q", want, cfg.AppleContainer.EnvdBinary)
	}
	if cfg.AppleContainer.EnvdPort != 49983 {
		t.Fatalf("expected applecontainer envd port %d, got %d", 49983, cfg.AppleContainer.EnvdPort)
	}
	if cfg.AppleContainer.HealthTimeoutSeconds != 60 {
		t.Fatalf("expected applecontainer health timeout %d, got %d", 60, cfg.AppleContainer.HealthTimeoutSeconds)
	}
	if cfg.AppleContainer.DefaultCPUs != 4 {
		t.Fatalf("expected applecontainer default cpus %d, got %d", 4, cfg.AppleContainer.DefaultCPUs)
	}
	if cfg.AppleContainer.DefaultMemoryMB != 2048 {
		t.Fatalf("expected applecontainer default memory %d, got %d", 2048, cfg.AppleContainer.DefaultMemoryMB)
	}
	template := cfg.AppleContainer.Templates["ubuntu-2404"]
	if template.Image != "docker.io/library/ubuntu:24.04" {
		t.Fatalf("expected template image, got %#v", template)
	}
	if template.CPUs != 2 {
		t.Fatalf("expected template cpus %d, got %d", 2, template.CPUs)
	}
	if template.MemoryMB != 1024 {
		t.Fatalf("expected template memory %d, got %d", 1024, template.MemoryMB)
	}
	if template.StartCmd != "sleep infinity" {
		t.Fatalf("expected template start command, got %#v", template)
	}
}

func TestAppleContainerRuntimeConfigValidate(t *testing.T) {
	validConfig := func() AppleContainerRuntimeConfig {
		return AppleContainerRuntimeConfig{
			ContainerNamePrefix:  "e2b-sandbox-",
			EnvdBinary:           "/tmp/e2b-local-envd",
			EnvdPort:             49983,
			HealthTimeoutSeconds: 60,
			DefaultCPUs:          4,
			DefaultMemoryMB:      1024,
			Templates: map[string]AppleContainerTemplateConfig{
				"alpine": {Image: "docker.io/library/alpine:3.20"},
			},
		}
	}

	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected valid applecontainer config, got %v", err)
	}

	prebakedConfig := validConfig()
	prebakedConfig.EnvdBinary = ""
	prebakedConfig.Templates = map[string]AppleContainerTemplateConfig{
		"alpine": {
			Image:            "docker.io/library/alpine:3.20",
			PrebakedEnvdPath: "/usr/local/bin/envd",
		},
	}
	if err := prebakedConfig.Validate(); err != nil {
		t.Fatalf("expected prebaked applecontainer config without envd binary to be valid, got %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*AppleContainerRuntimeConfig)
	}{
		{
			name:   "missing prefix",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.ContainerNamePrefix = "" },
		},
		{
			name:   "relative envd binary",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.EnvdBinary = "envd-bin/envd-linux-arm64" },
		},
		{
			name:   "zero envd port",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.EnvdPort = 0 },
		},
		{
			name:   "large envd port",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.EnvdPort = 70000 },
		},
		{
			name:   "non-positive health timeout",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.HealthTimeoutSeconds = 0 },
		},
		{
			name:   "non-positive default cpus",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.DefaultCPUs = 0 },
		},
		{
			name:   "non-positive default memory",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.DefaultMemoryMB = 0 },
		},
		{
			name:   "empty templates",
			mutate: func(cfg *AppleContainerRuntimeConfig) { cfg.Templates = nil },
		},
		{
			name: "template missing image",
			mutate: func(cfg *AppleContainerRuntimeConfig) {
				cfg.Templates = map[string]AppleContainerTemplateConfig{"alpine": {}}
			},
		},
		{
			name: "template relative prebaked envd path",
			mutate: func(cfg *AppleContainerRuntimeConfig) {
				cfg.EnvdBinary = ""
				cfg.Templates = map[string]AppleContainerTemplateConfig{
					"alpine": {Image: "docker.io/library/alpine:3.20", PrebakedEnvdPath: "usr/local/bin/envd"},
				}
			},
		},
		{
			name: "missing envd binary for non-prebaked template",
			mutate: func(cfg *AppleContainerRuntimeConfig) {
				cfg.EnvdBinary = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
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
