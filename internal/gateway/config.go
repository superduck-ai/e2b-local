package gateway

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultServerAddr  = "127.0.0.1:3000"
	defaultEnvdVersion = "99.99.99"

	defaultRuntimeType                = "docker"
	defaultDockerContainerNamePrefix  = "e2b-envd-"
	defaultDockerHealthTimeoutSeconds = 30
	defaultOrbstackMachineNamePrefix  = "e2b-sandbox-"
	defaultOrbstackDefaultMemory      = "2G"
	defaultOrbstackDefaultCPUs        = "2"
	defaultOrbstackDefaultDisk        = "16G"
	defaultOrbstackEnvdBinary         = "envd-bin/envd-linux-arm64"
	defaultOrbstackEnvdPort           = 49983
	defaultOrbstackHealthTimeout      = 60
	defaultTemplateBuildMaxConcurrent = 2
)

type Config struct {
	Server         ServerConfig          `yaml:"server"`
	Runtime        RuntimeConfig         `yaml:"runtime"`
	Docker         DockerRuntimeConfig   `yaml:"docker"`
	Orbstack       OrbstackRuntimeConfig `yaml:"orbstack"`
	TemplateBuilds TemplateBuildConfig   `yaml:"template_builds"`
}

type ServerConfig struct {
	Addr string `yaml:"addr"`
}

type RuntimeConfig struct {
	Type string `yaml:"type"`
}

type TemplateBuildConfig struct {
	MaxConcurrent int `yaml:"max_concurrent"`
}

type DockerRuntimeConfig struct {
	Host                 string `yaml:"host"`
	Platform             string `yaml:"platform"`
	ContainerNamePrefix  string `yaml:"container_name_prefix"`
	EnvdBinary           string `yaml:"envd_binary"`
	HealthTimeoutSeconds int    `yaml:"health_timeout_seconds"`
}

type OrbstackRuntimeConfig struct {
	MachineNamePrefix    string                            `yaml:"machine_name_prefix"`
	DefaultMemory        string                            `yaml:"default_memory"`
	DefaultCPUs          string                            `yaml:"default_cpus"`
	DefaultDisk          string                            `yaml:"default_disk"`
	Isolated             bool                              `yaml:"isolated"`
	EnvdBinary           string                            `yaml:"envd_binary"`
	EnvdPort             int                               `yaml:"envd_port"`
	HealthTimeoutSeconds int                               `yaml:"health_timeout_seconds"`
	VolumeHostPath       string                            `yaml:"volume_host_path"`
	Templates            map[string]OrbstackTemplateConfig `yaml:"templates"`
}

type OrbstackTemplateConfig struct {
	Memory      string `yaml:"memory"`
	CPUs        string `yaml:"cpus"`
	Disk        string `yaml:"disk"`
	StartCmd    string `yaml:"start_cmd"`
	BaseMachine string `yaml:"base_machine"`
}

func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Addr: defaultServerAddr,
		},
		Runtime: RuntimeConfig{
			Type: defaultRuntimeType,
		},
		Docker: DockerRuntimeConfig{
			Host:                 defaultDockerHost(),
			ContainerNamePrefix:  defaultDockerContainerNamePrefix,
			HealthTimeoutSeconds: defaultDockerHealthTimeoutSeconds,
		},
		Orbstack: OrbstackRuntimeConfig{
			MachineNamePrefix:    defaultOrbstackMachineNamePrefix,
			DefaultMemory:        defaultOrbstackDefaultMemory,
			DefaultCPUs:          defaultOrbstackDefaultCPUs,
			DefaultDisk:          defaultOrbstackDefaultDisk,
			EnvdBinary:           defaultBundledPath(defaultOrbstackEnvdBinary),
			EnvdPort:             defaultOrbstackEnvdPort,
			HealthTimeoutSeconds: defaultOrbstackHealthTimeout,
			VolumeHostPath:       defaultVolumeHostPath(),
		},
		TemplateBuilds: TemplateBuildConfig{
			MaxConcurrent: defaultTemplateBuildMaxConcurrent,
		},
	}
}

func defaultDockerHost() string {
	if value := strings.TrimSpace(os.Getenv("DOCKER_HOST")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		socket := filepath.Join(home, ".orbstack", "run", "docker.sock")
		if _, err := os.Stat(socket); err == nil {
			return "unix://" + socket
		}
	}
	return "unix:///var/run/docker.sock"
}

func defaultVolumeHostPath() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".e2b-local", "volumes")
	}
	return filepath.Join(".e2b-local", "volumes")
}

func defaultBundledPath(relPath string) string {
	relPath = filepath.Clean(strings.TrimSpace(relPath))
	if relPath == "." || relPath == "" || filepath.IsAbs(relPath) {
		return relPath
	}

	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		candidates = append(candidates, cwd)
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		candidates = append(candidates, filepath.Dir(exe))
	}

	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		for dir := filepath.Clean(candidate); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			if _, ok := seen[dir]; ok {
				break
			}
			seen[dir] = struct{}{}
			path := filepath.Join(dir, relPath)
			if _, err := os.Stat(path); err == nil {
				return path
			}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
		}
	}

	return relPath
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	if path == "" {
		cfg.ResolveLocalPaths("")
		return cfg, cfg.Validate()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.ResolveLocalPaths(filepath.Dir(absPathOrClean(path)))

	return cfg, cfg.Validate()
}

func absPathOrClean(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func (c *Config) ResolveLocalPaths(baseDir string) {
	if c == nil {
		return
	}
	c.Docker.EnvdBinary = resolveLocalPath(baseDir, c.Docker.EnvdBinary)
	c.Orbstack.EnvdBinary = resolveLocalPath(baseDir, c.Orbstack.EnvdBinary)
	c.Orbstack.VolumeHostPath = resolveLocalPath(baseDir, c.Orbstack.VolumeHostPath)
}

func resolveLocalPath(baseDir string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if expanded := expandHomePath(value); expanded != "" {
		value = expanded
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	if baseDir == "" {
		if cwd, err := os.Getwd(); err == nil && cwd != "" {
			baseDir = cwd
		}
	}
	if baseDir == "" {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(baseDir, value))
}

func expandHomePath(value string) string {
	if value != "~" && !strings.HasPrefix(value, "~/") {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	if value == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(value, "~/"))
}

func (c Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}

	if c.Runtime.Type == "" {
		return fmt.Errorf("runtime.type is required")
	}
	if c.TemplateBuilds.MaxConcurrent <= 0 {
		return fmt.Errorf("template_builds.max_concurrent must be greater than 0")
	}

	switch c.Runtime.Type {
	case "docker":
	case "orbstack":
	default:
		return fmt.Errorf("runtime.type must be docker or orbstack")
	}

	if c.Runtime.Type == "docker" {
		if err := c.Docker.Validate(); err != nil {
			return err
		}
	}

	if c.Runtime.Type == "orbstack" {
		if err := c.Orbstack.Validate(); err != nil {
			return err
		}
	}

	return nil
}

func (c DockerRuntimeConfig) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("docker.host is required")
	}

	if c.ContainerNamePrefix == "" {
		return fmt.Errorf("docker.container_name_prefix is required")
	}

	if c.EnvdBinary != "" && !filepath.IsAbs(c.EnvdBinary) {
		return fmt.Errorf("docker.envd_binary must be an absolute path")
	}

	if c.HealthTimeoutSeconds <= 0 {
		return fmt.Errorf("docker.health_timeout_seconds must be positive")
	}

	if _, err := url.ParseRequestURI(c.Host); err != nil {
		return fmt.Errorf("docker.host is invalid: %w", err)
	}

	return nil
}

func (c OrbstackRuntimeConfig) Validate() error {
	if strings.TrimSpace(c.MachineNamePrefix) == "" {
		return fmt.Errorf("orbstack.machine_name_prefix is required")
	}
	if strings.TrimSpace(c.DefaultMemory) == "" {
		return fmt.Errorf("orbstack.default_memory is required")
	}
	if strings.TrimSpace(c.DefaultCPUs) == "" {
		return fmt.Errorf("orbstack.default_cpus is required")
	}
	if strings.TrimSpace(c.DefaultDisk) == "" {
		return fmt.Errorf("orbstack.default_disk is required")
	}
	if strings.TrimSpace(c.EnvdBinary) == "" {
		return fmt.Errorf("orbstack.envd_binary is required")
	}
	if !filepath.IsAbs(c.EnvdBinary) {
		return fmt.Errorf("orbstack.envd_binary must be an absolute path")
	}
	if c.EnvdPort <= 0 {
		return fmt.Errorf("orbstack.envd_port must be positive")
	}
	if c.HealthTimeoutSeconds <= 0 {
		return fmt.Errorf("orbstack.health_timeout_seconds must be positive")
	}
	if strings.TrimSpace(c.VolumeHostPath) == "" {
		return fmt.Errorf("orbstack.volume_host_path is required")
	}
	if !filepath.IsAbs(c.VolumeHostPath) {
		return fmt.Errorf("orbstack.volume_host_path must be an absolute path")
	}
	for templateID, template := range c.Templates {
		if strings.TrimSpace(templateID) == "" {
			return fmt.Errorf("orbstack.templates keys must not be empty")
		}
		if value := strings.TrimSpace(template.BaseMachine); value != "" && strings.ContainsRune(value, '\n') {
			return fmt.Errorf("orbstack.templates.%s.base_machine must be a single machine name", templateID)
		}
	}
	return nil
}
