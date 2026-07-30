package orbstackbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"

	"github.com/docker/docker/errdefs"
	"github.com/google/uuid"
)

type RuntimeVolume = gateway.RuntimeVolume
type SandboxRecord = gateway.SandboxRecord
type SandboxRuntimeCreateRequest = gateway.SandboxRuntimeCreateRequest
type SandboxRuntimeInfo = gateway.SandboxRuntimeInfo
type SandboxRuntimeInspection = gateway.SandboxRuntimeInspection
type SnapshotListRequest = gateway.SnapshotListRequest
type VolumeMount = gateway.VolumeMount

const (
	envdServicePath      = "/etc/systemd/system/envd.service"
	envdBinaryPath       = "/usr/local/bin/envd"
	envdServiceWantsPath = "/etc/systemd/system/multi-user.target.wants/envd.service"
	sandboxMetadataPath  = "/var/lib/e2b-local/sandbox.json"
	volumeMetadataName   = ".e2b-meta.json"
	maxSandboxListLimit  = gateway.MaxSandboxListLimit
	minimumProvisionWait = 5 * time.Minute
)

type sandboxMetadata struct {
	SandboxID           string            `json:"sandbox_id"`
	TemplateID          string            `json:"template_id"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	EndAt               time.Time         `json:"end_at"`
	AllowInternetAccess *bool             `json:"allow_internet_access,omitempty"`
	VolumeMounts        []VolumeMount     `json:"volume_mounts,omitempty"`
}

type OrbstackRuntime struct {
	cfg          OrbstackRuntimeConfig
	vmClient     machineClient
	logger       *log.Logger
	httpClient   *http.Client
	checkHealthy func(ctx context.Context, envdURL string) error
	now          func() time.Time
	newID        func() string
}

type resolvedVolume struct {
	Volume  RuntimeVolume
	HostDir string
}

func init() {
	gateway.RegisterSandboxRuntimeFactory("orbstack", func(cfg gateway.Config, logger *log.Logger) (gateway.SandboxRuntime, error) {
		return NewOrbstackRuntime(cfg.Orbstack, logger)
	})
}

func NewOrbstackRuntime(cfg OrbstackRuntimeConfig, logger *log.Logger) (*OrbstackRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	runtime := &OrbstackRuntime{
		cfg:      cfg,
		vmClient: NewVMClient(logger),
		logger:   logger,
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
		},
		now:   time.Now,
		newID: uuid.NewString,
	}
	runtime.checkHealthy = runtime.defaultHealthCheck

	return runtime, nil
}

func (r *OrbstackRuntime) CreateSandbox(ctx context.Context, req SandboxRuntimeCreateRequest) (SandboxRuntimeInfo, error) {
	templateVM, template, err := r.resolveTemplate(ctx, req.TemplateID)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}

	volumeMounts, volumeHostDirs, err := r.resolveVolumeMounts(ctx, req.VolumeMounts)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}

	machineName := sandboxMachineName(r.cfg, req.SandboxID)
	spec := machineProvisionSpec{
		StartCmd:       strings.TrimSpace(template.StartCmd),
		EnvVars:        gateway.CopyStringMap(req.EnvVars),
		VolumeMounts:   volumeMounts,
		VolumeHostDirs: volumeHostDirs,
	}
	metadata := sandboxMetadata{
		SandboxID:           req.SandboxID,
		TemplateID:          req.TemplateID,
		Metadata:            gateway.CopyStringMap(req.Metadata),
		CreatedAt:           req.CreatedAt,
		EndAt:               req.EndAt,
		AllowInternetAccess: req.AllowInternetAccess,
		VolumeMounts:        volumeMounts,
	}

	created := false
	cleanupMachine := func() {
		if !created {
			return
		}
		if err := r.vmClient.DeleteVM(context.Background(), machineName); err != nil {
			r.logger.Printf("orbstack cleanup failed machine=%s error=%v", machineName, err)
		}
	}

	if err := r.vmClient.CloneVM(ctx, templateVM.Name, machineName); err != nil {
		return SandboxRuntimeInfo{}, err
	}
	created = true
	if err := r.prepareMachine(ctx, machineName, spec); err != nil {
		cleanupMachine()
		return SandboxRuntimeInfo{}, err
	}
	if err := r.vmClient.StartVM(ctx, machineName); err != nil {
		cleanupMachine()
		return SandboxRuntimeInfo{}, err
	}

	info, err := r.waitForMachineState(ctx, machineName, "running")
	if err != nil {
		cleanupMachine()
		return SandboxRuntimeInfo{}, err
	}
	if err := r.configureMachine(ctx, machineName, spec, metadata); err != nil {
		cleanupMachine()
		return SandboxRuntimeInfo{}, err
	}
	info, err = r.waitForMachineAddress(ctx, info.Name)
	if err != nil {
		cleanupMachine()
		return SandboxRuntimeInfo{}, err
	}
	if err := r.checkHealthy(ctx, vmEnvdURL(info, r.cfg.EnvdPort)); err != nil {
		cleanupMachine()
		return SandboxRuntimeInfo{}, err
	}
	runtimeInfo := r.runtimeInfo(req.SandboxID, info, volumeMounts)
	r.logger.Printf("orbstack sandbox started sandbox_id=%s machine=%s vm_id=%s envd_url=%s",
		req.SandboxID,
		info.Name,
		info.ID,
		runtimeInfo.EnvdURL,
	)
	return runtimeInfo, nil
}

func (r *OrbstackRuntime) prepareMachine(ctx context.Context, machineName string, spec machineProvisionSpec) error {
	mounts := dedupeVolumeMountsByVolumeID(spec.VolumeMounts)
	if r.cfg.Isolated || len(mounts) > 0 {
		if err := r.vmClient.SetMachineOption(ctx, machineName, "isolated", "true"); err != nil {
			return err
		}
	}
	for _, mount := range mounts {
		source := strings.TrimSpace(spec.VolumeHostDirs[mount.VolumeID])
		if source == "" {
			return fmt.Errorf("resolved host dir is missing for volume %s", mount.VolumeID)
		}
		dest := isolatedVolumeSourcePath(mount.VolumeID)
		if err := r.vmClient.AddMachineMount(ctx, machineName, source, dest); err != nil {
			return err
		}
	}
	return nil
}

func (r *OrbstackRuntime) DeleteSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	name := machineNameFromRuntimeInfo(info)
	if name == "" {
		return fmt.Errorf("machine is required")
	}
	return r.vmClient.DeleteVM(ctx, name)
}

func (r *OrbstackRuntime) PauseSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	name := machineNameFromRuntimeInfo(info)
	if name == "" {
		return fmt.Errorf("machine is required")
	}
	return r.vmClient.StopVM(ctx, name)
}

func (r *OrbstackRuntime) ResumeSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInfo, error) {
	name := machineNameFromRuntimeInfo(info)
	if name == "" {
		return SandboxRuntimeInfo{}, fmt.Errorf("machine is required")
	}
	if err := r.vmClient.StartVM(ctx, name); err != nil {
		return SandboxRuntimeInfo{}, err
	}

	vmInfo, err := r.waitForMachineState(ctx, name, "running")
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}
	vmInfo, err = r.waitForMachineAddress(ctx, vmInfo.Name)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}
	if err := r.checkHealthy(ctx, vmEnvdURL(vmInfo, r.cfg.EnvdPort)); err != nil {
		return SandboxRuntimeInfo{}, err
	}
	return r.runtimeInfo(info.SandboxID, vmInfo, info.VolumeMounts), nil
}

func (r *OrbstackRuntime) ListTemplates(ctx context.Context) ([]SandboxRuntimeTemplate, error) {
	vms, err := r.vmClient.ListVMs(ctx)
	if err != nil {
		return nil, err
	}

	templateVMs := listTemplateSourceVMs(vms, r.cfg)
	templates := make([]SandboxRuntimeTemplate, 0, len(templateVMs))
	for _, vmInfo := range templateVMs {
		fullInfo, err := r.vmClient.GetVMInfo(ctx, vmInfo.Name)
		if err != nil {
			if errors.Is(err, ErrVMNotFound) {
				continue
			}
			return nil, err
		}
		templates = append(templates, runtimeTemplateFromVM(r.cfg, fullInfo))
	}
	return templates, nil
}

func (r *OrbstackRuntime) InspectSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInspection, error) {
	name := machineNameFromRuntimeInfo(info)
	if name == "" {
		return SandboxRuntimeInspection{Info: info, Exists: false}, nil
	}

	vmInfo, err := r.vmClient.GetVMInfo(ctx, name)
	if err != nil {
		if errors.Is(err, ErrVMNotFound) {
			return SandboxRuntimeInspection{Info: info, Exists: false}, nil
		}
		return SandboxRuntimeInspection{}, err
	}

	return SandboxRuntimeInspection{
		Info:   r.runtimeInfo(info.SandboxID, vmInfo, info.VolumeMounts),
		State:  sandboxStateFromVMState(vmInfo.State),
		Exists: true,
	}, nil
}

func (r *OrbstackRuntime) RestoreSandboxes(ctx context.Context) ([]SandboxRecord, error) {
	vms, err := r.vmClient.ListVMs(ctx)
	if err != nil {
		return nil, err
	}

	sort.Slice(vms, func(i, j int) bool {
		return vms[i].Name < vms[j].Name
	})

	records := make([]SandboxRecord, 0, len(vms))
	for _, vmInfo := range vms {
		if !strings.HasPrefix(vmInfo.Name, strings.TrimSpace(r.cfg.MachineNamePrefix)) {
			continue
		}
		if strings.HasPrefix(vmInfo.Name, snapshotMachinePrefix(r.cfg)) {
			continue
		}
		fullInfo, err := r.vmClient.GetVMInfo(ctx, vmInfo.Name)
		if err != nil {
			if errors.Is(err, ErrVMNotFound) {
				continue
			}
			return nil, err
		}
		vmInfo = fullInfo

		metadata, err := r.readSandboxMetadata(ctx, vmInfo.Name)
		if err != nil {
			return nil, err
		}

		sandboxID := strings.TrimSpace(metadata.SandboxID)
		if sandboxID == "" {
			sandboxID = sandboxIDFromMachineName(r.cfg, vmInfo.Name)
		}
		templateID := strings.TrimSpace(metadata.TemplateID)
		createdAt := metadata.CreatedAt
		if createdAt.IsZero() {
			createdAt = r.now().UTC()
		}
		endAt := metadata.EndAt
		if endAt.IsZero() {
			endAt = createdAt.Add(time.Duration(gateway.DefaultSandboxTimeoutSeconds) * time.Second)
		}
		volumeMounts := normalizeVolumeMounts(metadata.VolumeMounts)
		runtimeInfo := r.runtimeInfo(sandboxID, vmInfo, volumeMounts)

		records = append(records, SandboxRecord{
			ID:                   sandboxID,
			TemplateID:           templateID,
			Metadata:             gateway.CopyStringMap(metadata.Metadata),
			EnvdURL:              runtimeInfo.EnvdURL,
			RuntimeInfo:          runtimeInfo,
			CreatedAt:            createdAt,
			EndAt:                endAt,
			State:                sandboxStateFromVMState(vmInfo.State),
			InternetAccessPolicy: gateway.InternetAccessPolicyFromBoolPtr(metadata.AllowInternetAccess),
		})
	}

	return records, nil
}

func (r *OrbstackRuntime) resolveTemplate(ctx context.Context, templateID string) (VMInfo, OrbstackTemplateConfig, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return VMInfo{}, OrbstackTemplateConfig{}, fmt.Errorf("templateID is required")
	}

	info, err := r.vmClient.GetVMInfo(ctx, templateID)
	if err != nil {
		if errors.Is(err, ErrVMNotFound) {
			return VMInfo{}, OrbstackTemplateConfig{}, fmt.Errorf("template %s not found", templateID)
		}
		return VMInfo{}, OrbstackTemplateConfig{}, err
	}

	if !isTemplateSourceMachine(r.cfg, info.Name) {
		return VMInfo{}, OrbstackTemplateConfig{}, fmt.Errorf("template %s not found", templateID)
	}

	return info, templateOverrides(r.cfg, info.Name), nil
}

func (r *OrbstackRuntime) CreateSandboxSnapshot(ctx context.Context, record SandboxRecord, req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) (e2bapi.SnapshotInfo, error) {
	source := machineNameFromRuntimeInfo(record.RuntimeInfo)
	if source == "" {
		return e2bapi.SnapshotInfo{}, fmt.Errorf("sandbox %s has no machine", record.ID)
	}

	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	createdAt := r.now().UTC()
	snapshotID := snapshotMachineName(r.cfg, record.ID, name, createdAt)
	if strings.TrimSpace(name) != "" {
		_ = r.vmClient.DeleteVM(ctx, snapshotID)
	}

	if err := r.vmClient.CloneVM(ctx, source, snapshotID); err != nil {
		return e2bapi.SnapshotInfo{}, err
	}

	return e2bapi.SnapshotInfo{
		SnapshotID: snapshotID,
		Names:      []string{snapshotID},
	}, nil
}

func (r *OrbstackRuntime) ListSnapshots(ctx context.Context, req SnapshotListRequest) ([]e2bapi.SnapshotInfo, error) {
	vms, err := r.vmClient.ListVMs(ctx)
	if err != nil {
		return nil, err
	}

	prefix := snapshotMachinePrefix(r.cfg)
	if sandboxID := strings.TrimSpace(req.SandboxID); sandboxID != "" {
		prefix += sandboxID + "-"
	}

	snapshots := make([]e2bapi.SnapshotInfo, 0, len(vms))
	for _, vmInfo := range vms {
		if !strings.HasPrefix(vmInfo.Name, prefix) {
			continue
		}
		snapshots = append(snapshots, e2bapi.SnapshotInfo{
			SnapshotID: vmInfo.Name,
			Names:      []string{vmInfo.Name},
		})
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].SnapshotID < snapshots[j].SnapshotID
	})

	start := 0
	nextToken := strings.TrimSpace(req.NextToken)
	if nextToken != "" {
		start = len(snapshots)
		for i, snapshot := range snapshots {
			if snapshot.SnapshotID == nextToken {
				start = i + 1
				break
			}
		}
	}
	if start > len(snapshots) {
		start = len(snapshots)
	}

	limit := req.Limit
	if limit <= 0 || limit > maxSandboxListLimit {
		limit = maxSandboxListLimit
	}
	end := start + limit
	if end > len(snapshots) {
		end = len(snapshots)
	}
	return append([]e2bapi.SnapshotInfo(nil), snapshots[start:end]...), nil
}

func (r *OrbstackRuntime) CreateVolume(ctx context.Context, name string) (RuntimeVolume, error) {
	if err := r.ensureVolumeStoreReady(); err != nil {
		return RuntimeVolume{}, err
	}

	volume := RuntimeVolume{
		VolumeID: r.newID(),
		Name:     strings.TrimSpace(name),
	}
	if volume.Name == "" {
		return RuntimeVolume{}, fmt.Errorf("name is required")
	}

	dir, err := createVolumeHostDir(r.cfg, volume.Name)
	if err != nil {
		return RuntimeVolume{}, err
	}

	if err := writeVolumeMetadata(dir, volume); err != nil {
		_ = os.RemoveAll(dir)
		return RuntimeVolume{}, err
	}

	return volume, nil
}

func (r *OrbstackRuntime) ListVolumes(ctx context.Context) ([]RuntimeVolume, error) {
	if err := r.ensureVolumeStoreReady(); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(strings.TrimSpace(r.cfg.VolumeHostPath))
	if err != nil {
		if os.IsNotExist(err) {
			return []RuntimeVolume{}, nil
		}
		return nil, fmt.Errorf("read volume base path: %w", err)
	}

	volumes := make([]RuntimeVolume, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		resolved, err := resolveVolumeDir(r.cfg, filepath.Join(strings.TrimSpace(r.cfg.VolumeHostPath), entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		volumes = append(volumes, resolved.Volume)
	}
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i].VolumeID < volumes[j].VolumeID
	})
	return volumes, nil
}

func (r *OrbstackRuntime) GetVolume(ctx context.Context, volumeID string) (RuntimeVolume, error) {
	if err := r.ensureVolumeStoreReady(); err != nil {
		return RuntimeVolume{}, err
	}
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return RuntimeVolume{}, errdefs.NotFound(fmt.Errorf("volume not found"))
	}

	resolved, err := findVolumeByID(r.cfg, volumeID)
	if err != nil {
		if os.IsNotExist(err) {
			return RuntimeVolume{}, errdefs.NotFound(fmt.Errorf("volume %s not found", volumeID))
		}
		return RuntimeVolume{}, err
	}
	return resolved.Volume, nil
}

func (r *OrbstackRuntime) DeleteVolume(ctx context.Context, volumeID string) (bool, error) {
	if err := r.ensureVolumeStoreReady(); err != nil {
		return false, err
	}
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return false, nil
	}

	resolved, err := findVolumeByID(r.cfg, volumeID)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	dir := resolved.HostDir
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat volume dir: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove volume dir: %w", err)
	}
	return true, nil
}

func (r *OrbstackRuntime) configureMachine(ctx context.Context, machineName string, spec machineProvisionSpec, metadata sandboxMetadata) error {
	envdBinary, err := os.ReadFile(r.cfg.EnvdBinary)
	if err != nil {
		return fmt.Errorf("read envd binary %s: %w", r.cfg.EnvdBinary, err)
	}
	service := renderEnvdService(r.cfg, spec.StartCmd, spec.EnvVars)
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode sandbox metadata: %w", err)
	}

	dirs := []string{
		parentDir(envdBinaryPath),
		parentDir(envdServicePath),
		parentDir(envdServiceWantsPath),
		parentDir(sandboxMetadataPath),
	}
	for _, dir := range dirs {
		if err := r.vmClient.MkdirAll(ctx, machineName, dir, 0o755); err != nil {
			return err
		}
	}
	if err := r.vmClient.WriteFile(ctx, machineName, envdBinaryPath, envdBinary, 0o755); err != nil {
		return err
	}
	if err := r.vmClient.WriteFile(ctx, machineName, envdServicePath, []byte(service), 0o644); err != nil {
		return err
	}
	if err := r.vmClient.WriteFile(ctx, machineName, sandboxMetadataPath, metadataJSON, 0o644); err != nil {
		return err
	}
	if err := r.configureVolumeMountFiles(ctx, machineName, spec); err != nil {
		return err
	}
	if err := r.vmClient.Symlink(ctx, machineName, "../envd.service", envdServiceWantsPath); err != nil {
		return err
	}
	_, err = r.vmClient.RunShell(ctx, machineName, strings.Join([]string{
		"set -eu",
		"sudo systemctl daemon-reload",
		"sudo systemctl enable envd",
		"sudo systemctl restart envd",
	}, "\n"))
	return err
}

func (r *OrbstackRuntime) configureVolumeMountFiles(ctx context.Context, machineName string, spec machineProvisionSpec) error {
	for _, mount := range normalizeVolumeMounts(spec.VolumeMounts) {
		target := strings.TrimSpace(mount.Path)
		volumeID := strings.TrimSpace(mount.VolumeID)
		if target == "" || volumeID == "" {
			continue
		}

		if err := r.vmClient.MkdirAll(ctx, machineName, parentDir(target), 0o755); err != nil {
			return err
		}
		source := sandboxVolumeSourcePath(r.cfg, volumeID, spec.VolumeHostDirs[volumeID])
		if err := r.vmClient.Symlink(ctx, machineName, source, target); err != nil {
			return err
		}
	}
	return nil
}

func (r *OrbstackRuntime) waitForMachineState(ctx context.Context, machine string, state string) (VMInfo, error) {
	deadline := r.now().UTC().Add(provisionWaitTimeout(r.cfg))
	for {
		info, err := r.vmClient.GetVMInfo(ctx, machine)
		if err == nil && strings.EqualFold(strings.TrimSpace(info.State), strings.TrimSpace(state)) {
			return info, nil
		}
		if err != nil && !errors.Is(err, ErrVMNotFound) {
			return VMInfo{}, err
		}
		if ctx.Err() != nil {
			return VMInfo{}, ctx.Err()
		}
		if r.now().UTC().After(deadline) {
			if err != nil {
				return VMInfo{}, fmt.Errorf("wait for machine %s state %s: %w", machine, state, err)
			}
			return VMInfo{}, fmt.Errorf("wait for machine %s state %s timed out", machine, state)
		}
		time.Sleep(1 * time.Second)
	}
}

func (r *OrbstackRuntime) waitForMachineAddress(ctx context.Context, machine string) (VMInfo, error) {
	deadline := r.now().UTC().Add(provisionWaitTimeout(r.cfg))
	var lastInfo VMInfo
	var lastErr error
	for {
		info, err := r.vmClient.GetVMInfo(ctx, machine)
		if err == nil {
			lastInfo = info
			if strings.EqualFold(strings.TrimSpace(info.State), "running") && preferredVMHost(info) != "" {
				return info, nil
			}
		} else if !errors.Is(err, ErrVMNotFound) {
			return VMInfo{}, err
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return VMInfo{}, ctx.Err()
		}
		if r.now().UTC().After(deadline) {
			if lastErr != nil {
				return VMInfo{}, fmt.Errorf("wait for machine %s address: %w", machine, lastErr)
			}
			if lastInfo.Name != "" {
				return VMInfo{}, fmt.Errorf("wait for machine %s address timed out", machine)
			}
			return VMInfo{}, fmt.Errorf("wait for machine %s address failed", machine)
		}
		time.Sleep(1 * time.Second)
	}
}

func (r *OrbstackRuntime) defaultHealthCheck(ctx context.Context, envdURL string) error {
	deadline := r.now().UTC().Add(time.Duration(r.cfg.HealthTimeoutSeconds) * time.Second)
	healthURL := strings.TrimRight(strings.TrimSpace(envdURL), "/") + "/health"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("build envd health request: %w", err)
		}
		resp, err := r.httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.now().UTC().After(deadline) {
			if err != nil {
				return fmt.Errorf("wait for envd health %s: %w", envdURL, err)
			}
			return fmt.Errorf("wait for envd health %s: unexpected status %d", envdURL, resp.StatusCode)
		}
		time.Sleep(1 * time.Second)
	}
}

func (r *OrbstackRuntime) runtimeInfo(sandboxID string, info VMInfo, volumeMounts []VolumeMount) SandboxRuntimeInfo {
	return SandboxRuntimeInfo{
		SandboxID:     sandboxID,
		EnvdURL:       vmEnvdURL(info, r.cfg.EnvdPort),
		ContainerID:   info.ID,
		ContainerName: info.Name,
		ContainerIP:   preferredVMHost(info),
		MachineID:     info.Name,
		VolumeMounts:  normalizeVolumeMounts(volumeMounts),
	}
}

func (r *OrbstackRuntime) readSandboxMetadata(ctx context.Context, machineName string) (sandboxMetadata, error) {
	output, err := r.vmClient.ReadFile(ctx, machineName, sandboxMetadataPath)
	if err != nil {
		if os.IsNotExist(err) || isRemoteFileNotFound(err) {
			return sandboxMetadata{}, nil
		}
		return sandboxMetadata{}, err
	}
	var metadata sandboxMetadata
	if err := json.Unmarshal(output, &metadata); err != nil {
		return sandboxMetadata{}, fmt.Errorf("decode sandbox metadata: %w", err)
	}
	return metadata, nil
}

func (r *OrbstackRuntime) resolveVolumeMounts(ctx context.Context, mounts []VolumeMount) ([]VolumeMount, map[string]string, error) {
	normalized := normalizeVolumeMounts(mounts)
	hostDirs := make(map[string]string, len(normalized))
	resolvedByID := make(map[string]resolvedVolume, len(normalized))
	for index, mount := range normalized {
		if strings.TrimSpace(mount.VolumeID) == "" {
			return nil, nil, fmt.Errorf("volume mount %d requires volumeID", index)
		}
		if strings.TrimSpace(mount.Path) == "" {
			return nil, nil, fmt.Errorf("volume mount %s requires path", mount.VolumeID)
		}
		if !strings.HasPrefix(strings.TrimSpace(mount.Path), "/") {
			return nil, nil, fmt.Errorf("volume mount path must be absolute: %s", mount.Path)
		}

		volumeID := strings.TrimSpace(mount.VolumeID)
		resolved, ok := resolvedByID[volumeID]
		if !ok {
			var err error
			resolved, err = findVolumeByID(r.cfg, volumeID)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, nil, fmt.Errorf("volume %s not found", volumeID)
				}
				return nil, nil, err
			}
			resolvedByID[volumeID] = resolved
			resolvedByID[resolved.Volume.VolumeID] = resolved
			hostDirs[resolved.Volume.VolumeID] = resolved.HostDir
		}
		normalized[index].VolumeID = resolved.Volume.VolumeID
		normalized[index].Name = resolved.Volume.Name
	}
	return normalized, hostDirs, nil
}

func (r *OrbstackRuntime) ensureVolumeStoreReady() error {
	base := strings.TrimSpace(r.cfg.VolumeHostPath)
	if base == "" {
		return fmt.Errorf("orbstack.volume_host_path is required")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fmt.Errorf("create volume host path %s: %w", base, err)
	}
	return nil
}

func machineNameFromRuntimeInfo(info SandboxRuntimeInfo) string {
	if value := strings.TrimSpace(info.MachineID); value != "" {
		return value
	}
	if value := strings.TrimSpace(info.ContainerName); value != "" {
		return value
	}
	return strings.TrimSpace(info.ContainerID)
}

func sandboxStateFromVMState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "stopped":
		return string(e2bapi.Paused)
	default:
		return string(e2bapi.Running)
	}
}

func normalizeVolumeMounts(volumeMounts []VolumeMount) []VolumeMount {
	result := make([]VolumeMount, 0, len(volumeMounts))
	for _, mount := range volumeMounts {
		name := strings.TrimSpace(mount.Name)
		volumeID := strings.TrimSpace(mount.VolumeID)
		pathValue := strings.TrimSpace(mount.Path)
		mountPath := strings.TrimSpace(mount.MountPath)
		if volumeID == "" {
			volumeID = name
		}
		if name == "" {
			name = volumeID
		}
		if pathValue == "" {
			pathValue = mountPath
		}
		if mountPath == "" {
			mountPath = pathValue
		}
		if volumeID == "" && pathValue == "" {
			continue
		}
		result = append(result, VolumeMount{
			Name:      name,
			Path:      pathValue,
			VolumeID:  volumeID,
			MountPath: mountPath,
		})
	}
	return result
}

func dedupeVolumeMountsByVolumeID(volumeMounts []VolumeMount) []VolumeMount {
	normalized := normalizeVolumeMounts(volumeMounts)
	if len(normalized) < 2 {
		return normalized
	}

	result := make([]VolumeMount, 0, len(normalized))
	seen := make(map[string]struct{}, len(normalized))
	for _, mount := range normalized {
		volumeID := strings.TrimSpace(mount.VolumeID)
		if volumeID == "" {
			continue
		}
		if _, ok := seen[volumeID]; ok {
			continue
		}
		seen[volumeID] = struct{}{}
		result = append(result, mount)
	}
	return result
}

func createVolumeHostDir(cfg OrbstackRuntimeConfig, volumeName string) (string, error) {
	baseName := volumeHostDirBaseName(volumeName)
	for index := 1; ; index++ {
		name := baseName
		if index > 1 {
			name = fmt.Sprintf("%s-%d", baseName, index)
		}
		dir := volumeBaseDir(cfg, name)
		err := os.Mkdir(dir, 0o777)
		if err == nil {
			if err := os.Chmod(dir, 0o777); err != nil {
				_ = os.RemoveAll(dir)
				return "", fmt.Errorf("chmod volume dir: %w", err)
			}
			return dir, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", fmt.Errorf("create volume dir: %w", err)
	}
}

func volumeBaseDir(cfg OrbstackRuntimeConfig, dirName string) string {
	return filepath.Join(strings.TrimSpace(cfg.VolumeHostPath), strings.TrimSpace(dirName))
}

func volumeHostDirBaseName(name string) string {
	name = strings.TrimSpace(name)
	replacer := strings.NewReplacer("/", "-", string(rune(0)), "")
	name = strings.TrimSpace(replacer.Replace(name))
	if name == "" || name == "." || name == ".." {
		return "volume"
	}
	return name
}

func volumeHostMetadataPath(hostDir string) string {
	return filepath.Join(strings.TrimSpace(hostDir), volumeMetadataName)
}

func encodeVolumeMetadata(volume RuntimeVolume) ([]byte, error) {
	data, err := json.Marshal(struct {
		VolumeID string `json:"VolumeID"`
		Name     string `json:"Name"`
	}{
		VolumeID: strings.TrimSpace(volume.VolumeID),
		Name:     strings.TrimSpace(volume.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("encode volume metadata: %w", err)
	}
	return data, nil
}

func decodeVolumeMetadata(volumeID string, data []byte) (RuntimeVolume, error) {
	var payload struct {
		VolumeID string `json:"VolumeID"`
		Name     string `json:"Name"`
		Token    string `json:"Token,omitempty"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return RuntimeVolume{}, fmt.Errorf("decode volume metadata %s: %w", volumeID, err)
	}
	return RuntimeVolume{
		VolumeID: strings.TrimSpace(payload.VolumeID),
		Name:     strings.TrimSpace(payload.Name),
	}, nil
}

func readLegacyVolumeMetadataFile(hostDir string) (RuntimeVolume, error) {
	data, err := os.ReadFile(volumeHostMetadataPath(hostDir))
	if err != nil {
		return RuntimeVolume{}, err
	}
	return decodeVolumeMetadata(filepath.Base(hostDir), data)
}

func findVolumeByID(cfg OrbstackRuntimeConfig, volumeID string) (resolvedVolume, error) {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return resolvedVolume{}, os.ErrNotExist
	}

	legacyDir := volumeBaseDir(cfg, volumeID)
	if info, err := os.Stat(legacyDir); err == nil && info.IsDir() {
		resolved, err := resolveVolumeDir(cfg, legacyDir)
		if err == nil && volumeMatches(resolved.Volume, volumeID) {
			return resolved, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return resolvedVolume{}, err
		}
	}

	entries, err := os.ReadDir(strings.TrimSpace(cfg.VolumeHostPath))
	if err != nil {
		if os.IsNotExist(err) {
			return resolvedVolume{}, os.ErrNotExist
		}
		return resolvedVolume{}, fmt.Errorf("read volume base path: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(strings.TrimSpace(cfg.VolumeHostPath), entry.Name())
		resolved, err := resolveVolumeDir(cfg, dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return resolvedVolume{}, err
		}
		if volumeMatches(resolved.Volume, volumeID) {
			return resolved, nil
		}
	}

	return resolvedVolume{}, os.ErrNotExist
}

func volumeMatches(volume RuntimeVolume, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(volume.VolumeID), value) || strings.EqualFold(strings.TrimSpace(volume.Name), value)
}

func resolveVolumeDir(cfg OrbstackRuntimeConfig, hostDir string) (resolvedVolume, error) {
	volume, err := readVolumeMetadata(hostDir)
	if err != nil {
		return resolvedVolume{}, err
	}

	volume.VolumeID = strings.TrimSpace(volume.VolumeID)
	volume.Name = strings.TrimSpace(volume.Name)
	if volume.VolumeID == "" {
		return resolvedVolume{}, fmt.Errorf("volume dir %s has empty volumeID metadata", hostDir)
	}
	if volume.Name == "" {
		volume.Name = volumeHostDirBaseName(filepath.Base(hostDir))
	}

	migratedDir, err := migrateLegacyVolumeDir(cfg, hostDir, volume)
	if err != nil {
		return resolvedVolume{}, err
	}

	return resolvedVolume{
		Volume:  volume,
		HostDir: migratedDir,
	}, nil
}

func migrateLegacyVolumeDir(cfg OrbstackRuntimeConfig, hostDir string, volume RuntimeVolume) (string, error) {
	if filepath.Base(hostDir) != strings.TrimSpace(volume.VolumeID) {
		return hostDir, nil
	}

	target, err := nextAvailableVolumeHostDir(cfg, volume.Name)
	if err != nil {
		return "", err
	}
	if filepath.Clean(target) == filepath.Clean(hostDir) {
		return hostDir, nil
	}
	if err := os.Rename(hostDir, target); err != nil {
		return "", fmt.Errorf("rename volume dir %s to %s: %w", hostDir, target, err)
	}
	return target, nil
}

func nextAvailableVolumeHostDir(cfg OrbstackRuntimeConfig, volumeName string) (string, error) {
	baseName := volumeHostDirBaseName(volumeName)
	for index := 1; ; index++ {
		name := baseName
		if index > 1 {
			name = fmt.Sprintf("%s-%d", baseName, index)
		}
		dir := volumeBaseDir(cfg, name)
		if _, err := os.Stat(dir); err == nil {
			continue
		} else if os.IsNotExist(err) {
			return dir, nil
		} else {
			return "", fmt.Errorf("stat volume dir %s: %w", dir, err)
		}
	}
}

func isRemoteFileNotFound(err error) bool {
	if err == nil {
		return false
	}
	value := strings.ToLower(err.Error())
	patterns := []string{
		"no such file",
		"not found",
		"cannot open",
	}
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func provisionWaitTimeout(cfg OrbstackRuntimeConfig) time.Duration {
	timeout := time.Duration(cfg.HealthTimeoutSeconds) * time.Second
	if timeout < minimumProvisionWait {
		return minimumProvisionWait
	}
	return timeout
}
