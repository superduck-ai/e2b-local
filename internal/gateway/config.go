package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultServerAddr  = "127.0.0.1:3000"
	defaultEnvdVersion = "99.99.99"

	defaultRuntimeType                 = "docker"
	defaultDockerContainerNamePrefix   = "e2b-envd-"
	defaultDockerPublishedHostIP       = "0.0.0.0"
	defaultDockerHealthTimeoutSeconds  = 30
	defaultOrbstackMachineNamePrefix   = "e2b-sandbox-"
	defaultOrbstackDefaultMemory       = "2G"
	defaultOrbstackDefaultCPUs         = "2"
	defaultOrbstackDefaultDisk         = "16G"
	defaultOrbstackEnvdBinary          = "envd-bin/envd-linux-arm64"
	defaultOrbstackEnvdPort            = 49983
	defaultOrbstackHealthTimeout       = 60
	defaultAppleContainerNamePrefix    = "e2b-sandbox-"
	defaultAppleContainerEnvdBinary    = "envd-bin/envd-linux-arm64"
	defaultAppleContainerEnvdPort      = 49983
	defaultAppleContainerHealthTimeout = 60
	defaultAppleContainerCPUs          = 4
	defaultAppleContainerMemoryMB      = 1024
	defaultTemplateBuildMaxConcurrent  = 2
	defaultTrafficProbeAddr            = "8.8.8.8:80"
)

var errRouteDetectionUnsupported = errors.New("route table detection is unsupported on this platform")

type Config struct {
	Server         ServerConfig                `yaml:"server"`
	Traffic        TrafficConfig               `yaml:"traffic"`
	Runtime        RuntimeConfig               `yaml:"runtime"`
	Docker         DockerRuntimeConfig         `yaml:"docker"`
	Orbstack       OrbstackRuntimeConfig       `yaml:"orbstack"`
	AppleContainer AppleContainerRuntimeConfig `yaml:"applecontainer"`
	TemplateBuilds TemplateBuildConfig         `yaml:"template_builds"`
}

type ServerConfig struct {
	Addr string `yaml:"addr"`
}

type TrafficConfig struct {
	AdvertisedHost      string `yaml:"advertised_host"`
	Interface           string `yaml:"interface"`
	AdvertisedProbeAddr string `yaml:"advertised_probe_addr"`
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
	PublishedPorts       []int  `yaml:"published_ports"`
	PublishedHostIP      string `yaml:"published_host_ip"`
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

type AppleContainerRuntimeConfig struct {
	ContainerNamePrefix  string                                  `yaml:"container_name_prefix"`
	EnvdBinary           string                                  `yaml:"envd_binary"`
	EnvdPort             int                                     `yaml:"envd_port"`
	HealthTimeoutSeconds int                                     `yaml:"health_timeout_seconds"`
	DefaultCPUs          int                                     `yaml:"default_cpus"`
	DefaultMemoryMB      int                                     `yaml:"default_memory_mb"`
	Templates            map[string]AppleContainerTemplateConfig `yaml:"templates"`
}

type AppleContainerTemplateConfig struct {
	Image            string `yaml:"image"`
	CPUs             int    `yaml:"cpus"`
	MemoryMB         int    `yaml:"memory_mb"`
	StartCmd         string `yaml:"start_cmd"`
	PrebakedEnvdPath string `yaml:"prebaked_envd_path"`
}

func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Addr: defaultServerAddr,
		},
		Traffic: TrafficConfig{
			AdvertisedProbeAddr: defaultTrafficProbeAddr,
		},
		Runtime: RuntimeConfig{
			Type: defaultRuntimeType,
		},
		Docker: DockerRuntimeConfig{
			Host:                 defaultDockerHost(),
			ContainerNamePrefix:  defaultDockerContainerNamePrefix,
			PublishedHostIP:      defaultDockerPublishedHostIP,
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
		AppleContainer: AppleContainerRuntimeConfig{
			ContainerNamePrefix:  defaultAppleContainerNamePrefix,
			EnvdBinary:           defaultBundledPath(defaultAppleContainerEnvdBinary),
			EnvdPort:             defaultAppleContainerEnvdPort,
			HealthTimeoutSeconds: defaultAppleContainerHealthTimeout,
			DefaultCPUs:          defaultAppleContainerCPUs,
			DefaultMemoryMB:      defaultAppleContainerMemoryMB,
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
	c.AppleContainer.EnvdBinary = resolveLocalPath(baseDir, c.AppleContainer.EnvdBinary)
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

	if err := c.Traffic.Validate(); err != nil {
		return err
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
	case "applecontainer":
	default:
		return fmt.Errorf("runtime.type must be docker, orbstack, or applecontainer")
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
	if c.Runtime.Type == "applecontainer" {
		if err := c.AppleContainer.Validate(); err != nil {
			return err
		}
	}

	return nil
}

func (c Config) ResolveTrafficAdvertisedHost() (Config, error) {
	if strings.TrimSpace(c.Traffic.AdvertisedHost) != "" {
		return c, nil
	}

	host, err := DetectTrafficAdvertisedHost(c.Traffic)
	if err != nil {
		return Config{}, err
	}
	c.Traffic.AdvertisedHost = host
	return c, nil
}

func DetectTrafficAdvertisedHost(cfg TrafficConfig) (string, error) {
	iface := strings.TrimSpace(cfg.Interface)
	if iface != "" {
		host, err := DetectInterfaceHost(iface)
		if err != nil {
			return "", fmt.Errorf("detect advertised host using traffic.interface %q: %w", iface, err)
		}
		return host, nil
	}

	host, routeErr := detectRouteOutboundHost(cfg.AdvertisedProbeAddr)
	if routeErr == nil {
		return host, nil
	}

	host, err := detectUDPOutboundHost(cfg.AdvertisedProbeAddr)
	if err != nil {
		if !errors.Is(routeErr, errRouteDetectionUnsupported) {
			return "", fmt.Errorf("%w; route table detection also failed: %v", err, routeErr)
		}
		return "", err
	}
	return host, nil
}

func DetectOutboundHost(probeAddr string) (string, error) {
	return DetectTrafficAdvertisedHost(TrafficConfig{AdvertisedProbeAddr: probeAddr})
}

func DetectInterfaceHost(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("interface name is required")
	}

	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", err
	}
	if iface.Flags&net.FlagUp == 0 {
		return "", fmt.Errorf("interface is not up")
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return "", fmt.Errorf("loopback interface is not a valid advertised interface")
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		ip := ipFromAddr(addr)
		if isUsableAdvertisedIP(ip) {
			return ip.To4().String(), nil
		}
	}
	return "", fmt.Errorf("interface has no usable IPv4 address")
}

func detectUDPOutboundHost(probeAddr string) (string, error) {
	probeAddr = strings.TrimSpace(probeAddr)
	if probeAddr == "" {
		return "", fmt.Errorf("traffic.advertised_probe_addr is required when traffic.advertised_host is empty")
	}

	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.Dial("udp", probeAddr)
	if err != nil {
		return "", fmt.Errorf("detect outbound host using %s: %w", probeAddr, err)
	}
	defer conn.Close()

	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || udpAddr == nil || udpAddr.IP == nil {
		return "", fmt.Errorf("detect outbound host using %s: local address is not UDP", probeAddr)
	}
	ip := udpAddr.IP
	if !isUsableAdvertisedIP(ip) {
		return "", fmt.Errorf("detect outbound host using %s returned non-routable address %s; set traffic.advertised_host explicitly", probeAddr, ip.String())
	}
	return ip.To4().String(), nil
}

func ipFromAddr(addr net.Addr) net.IP {
	switch value := addr.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		return nil
	}
}

func isUsableAdvertisedIP(ip net.IP) bool {
	if ip == nil || ip.To4() == nil {
		return false
	}
	return !ip.IsUnspecified() &&
		!ip.IsLoopback() &&
		!ip.IsMulticast() &&
		!ip.IsLinkLocalUnicast()
}

func (c TrafficConfig) Validate() error {
	if err := validateAdvertisedHost(c.AdvertisedHost); err != nil {
		return err
	}
	if err := validateInterfaceName(c.Interface); err != nil {
		return err
	}

	if strings.TrimSpace(c.AdvertisedProbeAddr) == "" {
		return fmt.Errorf("traffic.advertised_probe_addr is required")
	}
	if _, _, err := net.SplitHostPort(c.AdvertisedProbeAddr); err != nil {
		return fmt.Errorf("traffic.advertised_probe_addr must be host:port: %w", err)
	}
	return nil
}

func validateInterfaceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("traffic.interface must be a single interface name")
	}
	return nil
}

func validateAdvertisedHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("traffic.advertised_host must be a host or IP without scheme")
	}
	if strings.Contains(host, "/") {
		return fmt.Errorf("traffic.advertised_host must be a host or IP without path")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return fmt.Errorf("traffic.advertised_host must not include a port")
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

	if strings.TrimSpace(c.PublishedHostIP) == "" {
		return fmt.Errorf("docker.published_host_ip is required")
	}
	if ip := net.ParseIP(strings.TrimSpace(c.PublishedHostIP)); ip == nil {
		return fmt.Errorf("docker.published_host_ip must be an IP address")
	}

	for _, port := range c.PublishedPorts {
		if port <= 0 || port > 65535 {
			return fmt.Errorf("docker.published_ports values must be between 1 and 65535")
		}
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

func (c AppleContainerRuntimeConfig) Validate() error {
	if strings.TrimSpace(c.ContainerNamePrefix) == "" {
		return fmt.Errorf("applecontainer.container_name_prefix is required")
	}
	if strings.TrimSpace(c.EnvdBinary) != "" && !filepath.IsAbs(c.EnvdBinary) {
		return fmt.Errorf("applecontainer.envd_binary must be an absolute path")
	}
	if c.EnvdPort <= 0 || c.EnvdPort > 65535 {
		return fmt.Errorf("applecontainer.envd_port must be between 1 and 65535")
	}
	if c.HealthTimeoutSeconds <= 0 {
		return fmt.Errorf("applecontainer.health_timeout_seconds must be positive")
	}
	if c.DefaultCPUs <= 0 {
		return fmt.Errorf("applecontainer.default_cpus must be positive")
	}
	if c.DefaultMemoryMB <= 0 {
		return fmt.Errorf("applecontainer.default_memory_mb must be positive")
	}
	if len(c.Templates) == 0 {
		return fmt.Errorf("applecontainer.templates is required")
	}
	requiresHostEnvdBinary := false
	for templateID, template := range c.Templates {
		if strings.TrimSpace(templateID) == "" {
			return fmt.Errorf("applecontainer.templates keys must not be empty")
		}
		if strings.TrimSpace(template.Image) == "" {
			return fmt.Errorf("applecontainer.templates.%s.image is required", templateID)
		}
		if template.CPUs < 0 {
			return fmt.Errorf("applecontainer.templates.%s.cpus must not be negative", templateID)
		}
		if template.MemoryMB < 0 {
			return fmt.Errorf("applecontainer.templates.%s.memory_mb must not be negative", templateID)
		}
		if prebakedEnvdPath := strings.TrimSpace(template.PrebakedEnvdPath); prebakedEnvdPath != "" {
			if !path.IsAbs(prebakedEnvdPath) {
				return fmt.Errorf("applecontainer.templates.%s.prebaked_envd_path must be absolute", templateID)
			}
		} else {
			requiresHostEnvdBinary = true
		}
	}
	if requiresHostEnvdBinary && strings.TrimSpace(c.EnvdBinary) == "" {
		return fmt.Errorf("applecontainer.envd_binary is required unless every template sets prebaked_envd_path")
	}
	return nil
}
