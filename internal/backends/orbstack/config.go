package orbstackbackend

import (
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	gateway "e2b-local/internal/gateway"

	"github.com/docker/go-units"
)

type OrbstackRuntimeConfig = gateway.OrbstackRuntimeConfig
type OrbstackTemplateConfig = gateway.OrbstackTemplateConfig
type SandboxRuntimeTemplate = gateway.SandboxRuntimeTemplate

const isolatedVolumeMountRoot = "/mnt/e2b-local/volumes"

func templateOverrides(cfg OrbstackRuntimeConfig, templateID string) OrbstackTemplateConfig {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return OrbstackTemplateConfig{}
	}

	template, ok := cfg.Templates[templateID]
	if !ok {
		return OrbstackTemplateConfig{}
	}
	return template
}

func isTemplateSourceMachine(cfg OrbstackRuntimeConfig, machineName string) bool {
	machineName = strings.TrimSpace(machineName)
	if machineName == "" {
		return false
	}

	prefix := strings.TrimSpace(cfg.MachineNamePrefix)
	return prefix == "" || !strings.HasPrefix(machineName, prefix)
}

func listTemplateSourceVMs(vms []VMInfo, cfg OrbstackRuntimeConfig) []VMInfo {
	filtered := make([]VMInfo, 0, len(vms))
	seen := make(map[string]struct{}, len(vms))
	for _, info := range vms {
		name := strings.TrimSpace(info.Name)
		if !isTemplateSourceMachine(cfg, name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		filtered = append(filtered, info)
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Name < filtered[j].Name
	})

	return filtered
}

func runtimeTemplateFromVM(cfg OrbstackRuntimeConfig, info VMInfo) SandboxRuntimeTemplate {
	now := time.Now().UTC()
	templateID := strings.TrimSpace(info.Name)
	template := templateOverrides(cfg, templateID)
	memory := templateValue(template.Memory, cfg.DefaultMemory)
	disk := templateValue(template.Disk, cfg.DefaultDisk)
	cpus := templateValue(template.CPUs, cfg.DefaultCPUs)
	diskSizeMB := sizeMBOrDefault(disk, 0)
	if info.DiskSize > 0 {
		diskSizeMB = bytesToMB(info.DiskSize)
	}

	return SandboxRuntimeTemplate{
		TemplateID:  templateID,
		Names:       []string{templateID},
		ImageRef:    templateID,
		BuildCount:  1,
		BuildID:     templateBuildIDFromVM(info),
		BuildStatus: "ready",
		CPUCount:    intOrDefault(cpus, 1),
		DiskSizeMB:  diskSizeMB,
		MemoryMB:    sizeMBOrDefault(memory, 512),
		Public:      false,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func templateBuildIDFromVM(info VMInfo) string {
	if value := strings.TrimSpace(info.ID); value != "" {
		return value
	}
	return strings.TrimSpace(info.Name)
}

func templateValue(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func intOrDefault(value string, fallback int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func sizeMBOrDefault(value string, fallback int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := units.RAMInBytes(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return int((parsed + 1024*1024 - 1) / (1024 * 1024))
}

func bytesToMB(value int64) int {
	if value <= 0 {
		return 0
	}
	return int((value + 1024*1024 - 1) / (1024 * 1024))
}

func machineEnvdURL(name string, port int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return envdURLForHost(name+".orb.local", port)
}

func vmEnvdURL(info VMInfo, port int) string {
	if host := preferredVMHost(info); host != "" {
		return envdURLForHost(host, port)
	}
	return machineEnvdURL(info.Name, port)
}

func preferredVMHost(info VMInfo) string {
	if value := strings.TrimSpace(info.IP4); value != "" {
		return value
	}
	return strings.TrimSpace(info.IP6)
}

func envdURLForHost(host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func sandboxMachineName(cfg OrbstackRuntimeConfig, sandboxID string) string {
	return strings.TrimSpace(cfg.MachineNamePrefix) + strings.TrimSpace(sandboxID)
}

func snapshotMachinePrefix(cfg OrbstackRuntimeConfig) string {
	return strings.TrimRight(strings.TrimSpace(cfg.MachineNamePrefix), "-") + "-snapshot-"
}

func snapshotMachineName(cfg OrbstackRuntimeConfig, sandboxID string, name string, createdAt time.Time) string {
	component := safeMachineNameComponent(name)
	if component == "" {
		component = safeMachineNameComponent(strconv.FormatInt(createdAt.Unix(), 10))
	}
	if component == "" {
		component = "snapshot"
	}
	return snapshotMachinePrefix(cfg) + strings.TrimSpace(sandboxID) + "-" + component
}

func sandboxIDFromMachineName(cfg OrbstackRuntimeConfig, machineName string) string {
	return strings.TrimPrefix(strings.TrimSpace(machineName), strings.TrimSpace(cfg.MachineNamePrefix))
}

func safeMachineNameComponent(value string) string {
	var b strings.Builder
	lastDash := false

	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	result := strings.Trim(b.String(), "-")
	if len(result) > 48 {
		result = strings.Trim(result[:48], "-")
	}
	return result
}

func macSharedPath(hostPath string) string {
	hostPath = path.Clean(strings.TrimSpace(hostPath))
	if hostPath == "." || hostPath == "" {
		return ""
	}
	if !strings.HasPrefix(hostPath, "/") {
		hostPath = "/" + hostPath
	}
	return path.Join("/mnt/mac", hostPath)
}

func isolatedVolumeSourcePath(volumeID string) string {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return isolatedVolumeMountRoot
	}
	return path.Join(isolatedVolumeMountRoot, volumeID)
}

func sandboxVolumeSourcePath(cfg OrbstackRuntimeConfig, volumeID string, hostDir string) string {
	_ = cfg
	_ = hostDir
	return isolatedVolumeSourcePath(volumeID)
}
