//go:build darwin && cgo

package applecontainer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"

	"github.com/docker/docker/errdefs"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type AppleContainerRuntimeConfig = gateway.AppleContainerRuntimeConfig
type AppleContainerTemplateConfig = gateway.AppleContainerTemplateConfig

type appleContainerClient interface {
	Ping(ctx context.Context) error
	ResolveImage(ctx context.Context, ref string) (ImageDescription, error)
	ContainerCreate(ctx context.Context, config ContainerConfiguration) error
	ContainerBootstrap(ctx context.Context, id string) error
	ContainerStartProcess(ctx context.Context, containerID, processID string) error
	ContainerCopyIn(ctx context.Context, id, srcPath, dstPath string, mode uint32) error
	ContainerCreateProcess(ctx context.Context, containerID, processID string, config ProcessConfiguration) error
	ContainerStop(ctx context.Context, id string) error
	ContainerDelete(ctx context.Context, id string, force bool) error
	ContainerList(ctx context.Context, filters ContainerListFilters) ([]ContainerSnapshot, error)
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
	VolumeInspect(ctx context.Context, name string) (VolumeConfig, error)
	VolumeList(ctx context.Context) ([]VolumeConfig, error)
	VolumeDelete(ctx context.Context, name string) error
	DefaultNetworkAttachment(ctx context.Context, containerID string) ([]AttachmentConfig, error)
}

type AppleContainerRuntime struct {
	cfg          AppleContainerRuntimeConfig
	client       appleContainerClient
	logger       *log.Logger
	httpClient   *http.Client
	checkHealthy func(ctx context.Context, envdURL string) error
	findFreePort func() (int, error)
	now          func() time.Time
	newID        func() string
}

const (
	appleEnvdPath      = "/usr/local/bin/envd"
	appleEnvdProcessID = "envd"
	appleEnvdHost      = "127.0.0.1"
	appleInitSleepTime = "2147483647"

	appleLocalManagedLabel                = "e2b.local.managed"
	appleLocalImageLabel                  = "e2b.local.image"
	appleLocalSandboxIDLabel              = "e2b.local.sandbox_id"
	appleLocalSandboxTemplateIDLabel      = "e2b.local.template_id"
	appleLocalSandboxCreatedAtLabel       = "e2b.local.created_at"
	appleLocalSandboxEndAtLabel           = "e2b.local.end_at"
	appleLocalSandboxMetadataLabel        = "e2b.local.metadata"
	appleLocalSandboxAllowInternetLabel   = "e2b.local.allow_internet_access"
	appleLocalSandboxVolumeMountsLabel    = "e2b.local.volume_mounts"
	appleLocalSandboxEnvVarsLabel         = "e2b.local.env_vars"
	appleLocalVolumeIDLabel               = "e2b.local.volume_id"
	appleLocalVolumeNameLabel             = "e2b.local.name"
	appleResourceRoleLabel                = "com.apple.container.resource.role"
	appleResourceRoleBuiltin              = "builtin"
	defaultAppleContainerRuntimeHandler   = "container-runtime-linux"
	defaultAppleContainerVolumeNamePrefix = "e2b-vol-"
	maxAppleEnvdPortAttempts              = 3
	appleTransientNotFoundRetryAttempts   = 20
	appleTransientNotFoundInitialDelay    = 50 * time.Millisecond
	appleTransientNotFoundMaxDelay        = 500 * time.Millisecond
	appleContainerOperationLockPoll       = 100 * time.Millisecond
	appleContainerInspectRetryAttempts    = 10
	appleContainerInspectRetryDelay       = 100 * time.Millisecond
)

var _ gateway.SandboxRuntime = (*AppleContainerRuntime)(nil)
var _ gateway.VolumeRuntime = (*AppleContainerRuntime)(nil)
var _ gateway.SandboxRuntimeInspector = (*AppleContainerRuntime)(nil)
var _ gateway.SandboxRuntimeRestorer = (*AppleContainerRuntime)(nil)

func init() {
	gateway.RegisterSandboxRuntimeFactory("applecontainer", func(cfg gateway.Config, logger *log.Logger) (gateway.SandboxRuntime, error) {
		return NewAppleContainerRuntime(cfg.AppleContainer, logger)
	})
}

func NewAppleContainerRuntime(cfg AppleContainerRuntimeConfig, logger *log.Logger) (*AppleContainerRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	client, err := NewXPCClient()
	if err != nil {
		return nil, fmt.Errorf("connect to Apple Container XPC services; ensure Apple Container is installed and running: %w", err)
	}
	return &AppleContainerRuntime{
		cfg:        cfg,
		client:     client,
		logger:     logger,
		httpClient: &http.Client{Timeout: 3 * time.Second},
		findFreePort: func() (int, error) {
			return findFreePort()
		},
		now:   time.Now,
		newID: uuid.NewString,
	}, nil
}

func (r *AppleContainerRuntime) Close() {
	if r == nil {
		return
	}
	if closer, ok := r.client.(interface{ Close() }); ok {
		closer.Close()
	}
}

func (r *AppleContainerRuntime) CreateSandbox(ctx context.Context, req gateway.SandboxRuntimeCreateRequest) (gateway.SandboxRuntimeInfo, error) {
	releaseOperationLock, err := acquireAppleContainerOperationLock(ctx)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("acquire apple container operation lock: %w", err)
	}
	defer releaseOperationLock()

	templateID := strings.TrimSpace(req.TemplateID)
	if templateID == "" {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("templateID is required")
	}

	template, ok := r.cfg.Templates[templateID]
	if !ok {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("template %q not found", templateID)
	}

	imageRef := strings.TrimSpace(template.Image)
	image, err := r.client.ResolveImage(ctx, imageRef)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("resolve apple container image %q: %w", imageRef, err)
	}

	if strings.TrimSpace(template.PrebakedEnvdPath) == "" {
		if err := validateAppleEnvdBinary(r.cfg.EnvdBinary); err != nil {
			return gateway.SandboxRuntimeInfo{}, err
		}
	}

	volumeMounts, filesystems, err := r.resolveVolumeMounts(ctx, req.VolumeMounts)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, err
	}

	containerID := appleSandboxContainerID(r.cfg, req.SandboxID)
	networks, err := r.networkAttachments(ctx, containerID, req.AllowInternetAccess)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, err
	}

	labels, err := appleSandboxLabels(req, templateID, imageRef, volumeMounts)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, err
	}

	cpus := template.CPUs
	if cpus <= 0 {
		cpus = r.cfg.DefaultCPUs
	}
	memoryMB := template.MemoryMB
	if memoryMB <= 0 {
		memoryMB = r.cfg.DefaultMemoryMB
	}

	var lastErr error
	for attempt := 1; attempt <= maxAppleEnvdPortAttempts; attempt++ {
		hostPort, err := r.nextEnvdHostPort()
		if err != nil {
			return gateway.SandboxRuntimeInfo{}, fmt.Errorf("find free envd host port: %w", err)
		}

		config := ContainerConfiguration{
			ID:       containerID,
			Image:    image,
			Mounts:   filesystems,
			Labels:   labels,
			Networks: networks,
			DNS:      dnsConfiguration(req.AllowInternetAccess),
			InitProcess: ProcessConfiguration{
				Executable:         "/bin/sleep",
				Arguments:          []string{appleInitSleepTime},
				Environment:        processEnvironment(nil),
				WorkingDirectory:   "/",
				Terminal:           false,
				User:               ProcessUserRoot(),
				SupplementalGroups: []uint32{},
				Rlimits:            []any{},
			},
			PublishedPorts: []PublishPort{{
				HostAddress:   appleEnvdHost,
				HostPort:      uint16(hostPort),
				ContainerPort: uint16(r.cfg.EnvdPort),
				Proto:         "tcp",
				Count:         1,
			}},
			Resources: Resources{
				CPUs:        cpus,
				MemoryBytes: uint64(memoryMB) * 1024 * 1024,
				CPUOverhead: 1,
			},
			RuntimeHandler: defaultAppleContainerRuntimeHandler,
		}

		if err := r.client.ContainerCreate(ctx, config); err != nil {
			lastErr = fmt.Errorf("create apple container %s: %w", containerID, err)
			if isPortAllocationError(err) {
				if attempt < maxAppleEnvdPortAttempts {
					r.cleanupContainer(containerID)
					r.logger.Printf("applecontainer retrying sandbox create after host port conflict sandbox_id=%s container_id=%s attempt=%d", req.SandboxID, containerID, attempt)
					continue
				}
				return gateway.SandboxRuntimeInfo{}, fmt.Errorf("create apple container %s after %d host port attempts: %w", containerID, maxAppleEnvdPortAttempts, err)
			}
			return gateway.SandboxRuntimeInfo{}, lastErr
		}

		cleanup := func() {
			r.cleanupContainer(containerID)
		}

		if err := r.client.ContainerBootstrap(ctx, containerID); err != nil {
			cleanup()
			return gateway.SandboxRuntimeInfo{}, fmt.Errorf("bootstrap apple container %s: %w", containerID, err)
		}
		if err := r.startEnvd(ctx, containerID, req.EnvVars, template); err != nil {
			cleanup()
			lastErr = err
			if isAppleNotFound(err) && attempt < maxAppleEnvdPortAttempts {
				r.logger.Printf("applecontainer retrying sandbox create after transient not_found sandbox_id=%s container_id=%s attempt=%d", req.SandboxID, containerID, attempt)
				continue
			}
			return gateway.SandboxRuntimeInfo{}, err
		}

		envdURL := appleEnvdURL(strconv.Itoa(hostPort))
		if err := r.healthCheck(ctx, envdURL); err != nil {
			cleanup()
			return gateway.SandboxRuntimeInfo{}, fmt.Errorf("envd health check: %w", err)
		}

		r.logger.Printf("applecontainer sandbox started sandbox_id=%s container_id=%s envd_url=%s", req.SandboxID, containerID, envdURL)

		return gateway.SandboxRuntimeInfo{
			SandboxID:     req.SandboxID,
			EnvdURL:       envdURL,
			ContainerID:   containerID,
			ContainerName: containerID,
			HostPort:      strconv.Itoa(hostPort),
			MachineID:     containerID,
			VolumeMounts:  volumeMounts,
		}, nil
	}

	return gateway.SandboxRuntimeInfo{}, lastErr
}

func (r *AppleContainerRuntime) ListTemplates(ctx context.Context) ([]gateway.SandboxRuntimeTemplate, error) {
	templates := make([]gateway.SandboxRuntimeTemplate, 0, len(r.cfg.Templates))
	ids := make([]string, 0, len(r.cfg.Templates))
	for id := range r.cfg.Templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := r.now
	if now == nil {
		now = time.Now
	}
	listedAt := now().UTC()
	for _, id := range ids {
		template := r.cfg.Templates[id]
		imageRef := strings.TrimSpace(template.Image)
		buildID := imageRef
		if buildID == "" {
			buildID = id
		}
		cpus := template.CPUs
		if cpus <= 0 {
			cpus = r.cfg.DefaultCPUs
		}
		memoryMB := template.MemoryMB
		if memoryMB <= 0 {
			memoryMB = r.cfg.DefaultMemoryMB
		}
		templates = append(templates, gateway.SandboxRuntimeTemplate{
			TemplateID:  id,
			Names:       []string{id},
			ImageRef:    imageRef,
			BuildCount:  1,
			BuildID:     buildID,
			BuildStatus: string(e2bapi.TemplateBuildStatusReady),
			CPUCount:    cpus,
			MemoryMB:    memoryMB,
			Public:      false,
			CreatedAt:   listedAt,
			UpdatedAt:   listedAt,
		})
	}
	return templates, nil
}

func (r *AppleContainerRuntime) DeleteSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) error {
	containerID := appleContainerIDFromRuntimeInfo(info)
	if containerID == "" {
		return nil
	}
	if err := r.client.ContainerDelete(ctx, containerID, true); err != nil {
		if isAppleNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func (r *AppleContainerRuntime) PauseSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) error {
	containerID := appleContainerIDFromRuntimeInfo(info)
	if containerID == "" {
		return nil
	}
	if err := r.client.ContainerStop(ctx, containerID); err != nil {
		if isAppleAlreadyStopped(err) {
			return nil
		}
		return err
	}
	return nil
}

func (r *AppleContainerRuntime) ResumeSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) (gateway.SandboxRuntimeInfo, error) {
	containerID := appleContainerIDFromRuntimeInfo(info)
	if containerID == "" {
		return info, nil
	}

	snapshot, err := r.containerSnapshot(ctx, containerID)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, err
	}
	hostPort := strings.TrimSpace(info.HostPort)
	if hostPort == "" {
		hostPort = envdHostPort(snapshot.Configuration.PublishedPorts, r.cfg.EnvdPort)
	}
	if hostPort == "" {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("missing persisted envd host port for apple container %s", containerID)
	}

	if e2bStateFromAppleStatus(snapshot.Status) == string(e2bapi.Running) {
		info = r.runtimeInfoFromSnapshot(snapshot, info)
		if info.EnvdURL == "" {
			info.EnvdURL = appleEnvdURL(hostPort)
		}
		if err := r.healthCheck(ctx, info.EnvdURL); err != nil {
			return gateway.SandboxRuntimeInfo{}, fmt.Errorf("envd health check: %w", err)
		}
		return info, nil
	}

	template := r.templateForSnapshot(snapshot)
	envVars := stringMapFromLabel(snapshot.Configuration.Labels[appleLocalSandboxEnvVarsLabel])
	releaseOperationLock, err := acquireAppleContainerOperationLock(ctx)
	if err != nil {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("acquire apple container operation lock: %w", err)
	}
	defer releaseOperationLock()

	if err := r.client.ContainerBootstrap(ctx, containerID); err != nil {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("bootstrap apple container %s: %w", containerID, err)
	}
	if err := r.startEnvd(ctx, containerID, envVars, template); err != nil {
		return gateway.SandboxRuntimeInfo{}, err
	}

	envdURL := appleEnvdURL(hostPort)
	if err := r.healthCheck(ctx, envdURL); err != nil {
		return gateway.SandboxRuntimeInfo{}, fmt.Errorf("envd health check: %w", err)
	}

	info.SandboxID = firstNonEmpty(info.SandboxID, snapshot.Configuration.Labels[appleLocalSandboxIDLabel])
	info.EnvdURL = envdURL
	info.ContainerID = containerID
	info.ContainerName = containerID
	info.HostPort = hostPort
	info.MachineID = containerID
	if len(info.VolumeMounts) == 0 {
		info.VolumeMounts = appleVolumeMountsFromLabels(snapshot.Configuration.Labels)
	}
	return info, nil
}

func (r *AppleContainerRuntime) CreateVolume(ctx context.Context, name string) (gateway.RuntimeVolume, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return gateway.RuntimeVolume{}, fmt.Errorf("volume name is required")
	}

	volumeID := strings.TrimSpace(r.newID())
	if volumeID == "" {
		volumeID = uuid.NewString()
	}
	volumeName := appleVolumeName(volumeID)
	labels := map[string]string{
		appleLocalManagedLabel:    "true",
		appleLocalVolumeIDLabel:   volumeID,
		appleLocalVolumeNameLabel: name,
	}
	if err := r.client.VolumeCreate(ctx, volumeName, labels); err != nil {
		return gateway.RuntimeVolume{}, fmt.Errorf("create apple container volume %s: %w", name, err)
	}
	return gateway.RuntimeVolume{VolumeID: volumeID, Name: name}, nil
}

func (r *AppleContainerRuntime) ListVolumes(ctx context.Context) ([]gateway.RuntimeVolume, error) {
	volumes, err := r.client.VolumeList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list apple container volumes: %w", err)
	}
	result := make([]gateway.RuntimeVolume, 0, len(volumes))
	for _, volume := range volumes {
		if volume.Labels[appleLocalManagedLabel] != "true" {
			continue
		}
		runtimeVolume := runtimeVolumeFromAppleVolume(volume)
		if runtimeVolume.VolumeID == "" {
			continue
		}
		result = append(result, runtimeVolume)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].VolumeID < result[j].VolumeID
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (r *AppleContainerRuntime) GetVolume(ctx context.Context, volumeID string) (gateway.RuntimeVolume, error) {
	volume, ok, err := r.findRuntimeVolume(ctx, volumeID)
	if err != nil {
		return gateway.RuntimeVolume{}, err
	}
	if !ok {
		return gateway.RuntimeVolume{}, errdefs.NotFound(fmt.Errorf("volume %s not found", strings.TrimSpace(volumeID)))
	}
	return volume, nil
}

func (r *AppleContainerRuntime) DeleteVolume(ctx context.Context, volumeID string) (bool, error) {
	volume, ok, err := r.findRuntimeVolume(ctx, volumeID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := r.client.VolumeDelete(ctx, appleVolumeName(volume.VolumeID)); err != nil {
		if isAppleNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("delete apple container volume %s: %w", volume.VolumeID, err)
	}
	return true, nil
}

func (r *AppleContainerRuntime) InspectSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) (gateway.SandboxRuntimeInspection, error) {
	containerID := appleContainerIDFromRuntimeInfo(info)
	if containerID == "" {
		return gateway.SandboxRuntimeInspection{Info: info, Exists: false}, nil
	}

	snapshot, exists, err := r.inspectContainerSnapshot(ctx, containerID)
	if err != nil {
		return gateway.SandboxRuntimeInspection{}, err
	}
	if !exists {
		return gateway.SandboxRuntimeInspection{Info: info, Exists: false}, nil
	}

	info = r.runtimeInfoFromSnapshot(snapshot, info)
	return gateway.SandboxRuntimeInspection{
		Info:   info,
		State:  e2bStateFromAppleStatus(snapshot.Status),
		Exists: true,
	}, nil
}

func (r *AppleContainerRuntime) inspectContainerSnapshot(ctx context.Context, containerID string) (ContainerSnapshot, bool, error) {
	for attempt := 1; attempt <= appleContainerInspectRetryAttempts; attempt++ {
		snapshots, err := r.client.ContainerList(ctx, ContainerListFilters{IDs: []string{containerID}})
		if err != nil {
			return ContainerSnapshot{}, false, err
		}
		if len(snapshots) > 0 {
			return snapshots[0], true, nil
		}
		if attempt == appleContainerInspectRetryAttempts {
			break
		}
		timer := time.NewTimer(appleContainerInspectRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ContainerSnapshot{}, false, ctx.Err()
		case <-timer.C:
		}
	}
	return ContainerSnapshot{}, false, nil
}

func (r *AppleContainerRuntime) RestoreSandboxes(ctx context.Context) ([]gateway.SandboxRecord, error) {
	snapshots, err := r.client.ContainerList(ctx, ContainerListFilters{
		Labels: map[string]string{appleLocalManagedLabel: "true"},
	})
	if err != nil {
		return nil, fmt.Errorf("list apple container sandboxes: %w", err)
	}

	records := make([]gateway.SandboxRecord, 0, len(snapshots))
	for _, snapshot := range snapshots {
		labels := snapshot.Configuration.Labels
		sandboxID := strings.TrimSpace(labels[appleLocalSandboxIDLabel])
		if sandboxID == "" {
			continue
		}
		state := e2bStateFromAppleStatus(snapshot.Status)
		if state == "" {
			continue
		}
		info := r.runtimeInfoFromSnapshot(snapshot, gateway.SandboxRuntimeInfo{SandboxID: sandboxID})
		createdAt := timeFromLabel(labels[appleLocalSandboxCreatedAtLabel])
		if createdAt.IsZero() && snapshot.Configuration.CreationDate != nil {
			createdAt = time.Time(*snapshot.Configuration.CreationDate).UTC()
		}
		if createdAt.IsZero() {
			createdAt = r.now().UTC()
		}
		endAt := timeFromLabel(labels[appleLocalSandboxEndAtLabel])
		if endAt.IsZero() {
			endAt = createdAt.Add(time.Duration(gateway.DefaultSandboxTimeoutSeconds) * time.Second)
		}

		records = append(records, gateway.SandboxRecord{
			ID:                  sandboxID,
			TemplateID:          strings.TrimSpace(labels[appleLocalSandboxTemplateIDLabel]),
			Metadata:            stringMapFromLabel(labels[appleLocalSandboxMetadataLabel]),
			EnvdURL:             info.EnvdURL,
			RuntimeInfo:         info,
			CreatedAt:           createdAt,
			EndAt:               endAt,
			State:               state,
			AllowInternetAccess: boolPtrFromLabel(labels[appleLocalSandboxAllowInternetLabel]),
		})
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

func validateAppleEnvdBinary(path string) error {
	stat, err := os.Stat(strings.TrimSpace(path))
	if err != nil {
		return fmt.Errorf("applecontainer.envd_binary is not accessible: %w", err)
	}
	if stat.IsDir() {
		return fmt.Errorf("applecontainer.envd_binary is a directory: %s", path)
	}
	if stat.Mode()&0o111 == 0 {
		return fmt.Errorf("applecontainer.envd_binary is not executable: %s", path)
	}
	return nil
}

func findFreePort() (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(appleEnvdHost, "0"))
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return addr.Port, nil
}

func appleSandboxContainerID(cfg AppleContainerRuntimeConfig, sandboxID string) string {
	return strings.TrimSpace(cfg.ContainerNamePrefix) + strings.TrimSpace(sandboxID)
}

func appleContainerIDFromRuntimeInfo(info gateway.SandboxRuntimeInfo) string {
	if value := strings.TrimSpace(info.ContainerID); value != "" {
		return value
	}
	if value := strings.TrimSpace(info.ContainerName); value != "" {
		return value
	}
	return strings.TrimSpace(info.MachineID)
}

func (r *AppleContainerRuntime) networkAttachments(ctx context.Context, containerID string, allowInternet *bool) ([]AttachmentConfig, error) {
	if allowInternet != nil && !*allowInternet {
		return nil, nil
	}
	attachments, err := r.client.DefaultNetworkAttachment(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("resolve apple container default network: %w", err)
	}
	return attachments, nil
}

func dnsConfiguration(allowInternet *bool) *DNSConfiguration {
	if allowInternet != nil && !*allowInternet {
		return nil
	}
	return &DNSConfiguration{
		Nameservers:   []string{"1.1.1.1"},
		SearchDomains: []string{},
		Options:       []string{},
	}
}

func (r *AppleContainerRuntime) startEnvd(ctx context.Context, containerID string, envVars map[string]string, template AppleContainerTemplateConfig) error {
	if err := r.withAppleTransientNotFoundRetry(ctx, "start init process", func(ctx context.Context) error {
		return r.client.ContainerStartProcess(ctx, containerID, containerID)
	}); err != nil {
		return fmt.Errorf("start apple container init process: %w", err)
	}
	executable := strings.TrimSpace(template.PrebakedEnvdPath)
	if executable == "" {
		executable = appleEnvdPath
		if err := r.withAppleTransientNotFoundRetry(ctx, "copy envd binary", func(ctx context.Context) error {
			return r.client.ContainerCopyIn(ctx, containerID, r.cfg.EnvdBinary, appleEnvdPath, 0o755)
		}); err != nil {
			return fmt.Errorf("copy envd binary: %w", err)
		}
	}

	config := ProcessConfiguration{
		Executable:         executable,
		Arguments:          envdArguments(r.cfg.EnvdPort, template.StartCmd),
		Environment:        processEnvironment(envVars),
		WorkingDirectory:   "/",
		Terminal:           false,
		User:               ProcessUserRoot(),
		SupplementalGroups: []uint32{},
		Rlimits:            []any{},
	}
	if err := r.withAppleTransientNotFoundRetry(ctx, "create envd process", func(ctx context.Context) error {
		return r.client.ContainerCreateProcess(ctx, containerID, appleEnvdProcessID, config)
	}); err != nil {
		return fmt.Errorf("create envd process: %w", err)
	}
	if err := r.withAppleTransientNotFoundRetry(ctx, "start envd process", func(ctx context.Context) error {
		return r.client.ContainerStartProcess(ctx, containerID, appleEnvdProcessID)
	}); err != nil {
		return fmt.Errorf("start envd process: %w", err)
	}
	return nil
}

func (r *AppleContainerRuntime) withAppleTransientNotFoundRetry(ctx context.Context, operation string, execute func(context.Context) error) error {
	var lastErr error
	delay := appleTransientNotFoundInitialDelay
	for attempt := 1; attempt <= appleTransientNotFoundRetryAttempts; attempt++ {
		if err := execute(ctx); err != nil {
			if !isAppleNotFound(err) {
				return err
			}
			lastErr = err
		} else {
			if attempt > 1 {
				r.logger.Printf("applecontainer recovered after transient not_found operation=%s attempts=%d", operation, attempt)
			}
			return nil
		}

		if attempt == appleTransientNotFoundRetryAttempts {
			break
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}

		delay *= 2
		if delay > appleTransientNotFoundMaxDelay {
			delay = appleTransientNotFoundMaxDelay
		}
	}
	return lastErr
}

func acquireAppleContainerOperationLock(ctx context.Context) (func(), error) {
	lockPath := filepath.Join(os.TempDir(), "e2b-applecontainer-operation.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
				_ = lockFile.Close()
			}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = lockFile.Close()
			return nil, err
		}

		timer := time.NewTimer(appleContainerOperationLockPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			_ = lockFile.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *AppleContainerRuntime) nextEnvdHostPort() (int, error) {
	find := r.findFreePort
	if find == nil {
		find = findFreePort
	}
	hostPort, err := find()
	if err != nil {
		return 0, err
	}
	if hostPort <= 0 || hostPort > 65535 {
		return 0, fmt.Errorf("invalid envd host port %d", hostPort)
	}
	return hostPort, nil
}

func (r *AppleContainerRuntime) cleanupContainer(containerID string) {
	if err := r.client.ContainerDelete(context.Background(), containerID, true); err != nil {
		if isAppleNotFound(err) {
			return
		}
		r.logger.Printf("applecontainer cleanup failed container_id=%s error=%v", containerID, err)
	}
}

func isPortAllocationError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	portConflictSignals := []string{
		"address already in use",
		"bind: address",
		"host port",
		"port already allocated",
		"port is already allocated",
		"port is already in use",
		"port is unavailable",
	}
	for _, signal := range portConflictSignals {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func envdArguments(port int, startCmd string) []string {
	args := []string{"-isnotfc", "-port", strconv.Itoa(port)}
	if strings.TrimSpace(startCmd) != "" {
		args = append(args, "-cmd", strings.TrimSpace(startCmd))
	}
	return args
}

func processEnvironment(values map[string]string) []string {
	env := map[string]string{
		"HOME": "/root",
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

func appleSandboxLabels(req gateway.SandboxRuntimeCreateRequest, templateID string, imageRef string, mounts []gateway.VolumeMount) (map[string]string, error) {
	labels := map[string]string{
		appleLocalManagedLabel:           "true",
		appleLocalImageLabel:             imageRef,
		appleLocalSandboxIDLabel:         strings.TrimSpace(req.SandboxID),
		appleLocalSandboxTemplateIDLabel: strings.TrimSpace(templateID),
	}
	if !req.CreatedAt.IsZero() {
		labels[appleLocalSandboxCreatedAtLabel] = req.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if !req.EndAt.IsZero() {
		labels[appleLocalSandboxEndAtLabel] = req.EndAt.UTC().Format(time.RFC3339Nano)
	}
	if len(req.Metadata) > 0 {
		data, err := json.Marshal(req.Metadata)
		if err != nil {
			return nil, fmt.Errorf("encode sandbox metadata label: %w", err)
		}
		labels[appleLocalSandboxMetadataLabel] = string(data)
	}
	if req.AllowInternetAccess != nil {
		labels[appleLocalSandboxAllowInternetLabel] = strconv.FormatBool(*req.AllowInternetAccess)
	}
	if len(mounts) > 0 {
		data, err := json.Marshal(mounts)
		if err != nil {
			return nil, fmt.Errorf("encode sandbox volume mounts label: %w", err)
		}
		labels[appleLocalSandboxVolumeMountsLabel] = string(data)
	}
	if len(req.EnvVars) > 0 {
		data, err := json.Marshal(req.EnvVars)
		if err != nil {
			return nil, fmt.Errorf("encode sandbox env vars label: %w", err)
		}
		labels[appleLocalSandboxEnvVarsLabel] = string(data)
	}
	for key, value := range labels {
		if strings.TrimSpace(value) == "" {
			delete(labels, key)
		}
	}
	return labels, nil
}

func (r *AppleContainerRuntime) resolveVolumeMounts(ctx context.Context, mounts []gateway.VolumeMount) ([]gateway.VolumeMount, []Filesystem, error) {
	normalized := normalizeVolumeMounts(mounts)
	filesystems := make([]Filesystem, 0, len(normalized))
	for index, mount := range normalized {
		if strings.TrimSpace(mount.VolumeID) == "" && strings.TrimSpace(mount.Name) == "" {
			return nil, nil, fmt.Errorf("volume mount %d requires volumeID or name", index)
		}
		if strings.TrimSpace(mount.MountPath) == "" {
			return nil, nil, fmt.Errorf("volume mount %d requires mount path", index)
		}
		if !strings.HasPrefix(strings.TrimSpace(mount.MountPath), "/") {
			return nil, nil, fmt.Errorf("volume mount path must be absolute: %s", mount.MountPath)
		}

		volume, err := r.resolveVolumeForMount(ctx, mount)
		if err != nil {
			return nil, nil, err
		}
		runtimeVolume := runtimeVolumeFromAppleVolume(volume)
		normalized[index].VolumeID = runtimeVolume.VolumeID
		normalized[index].Name = runtimeVolume.Name
		filesystems = append(filesystems, VolumeFilesystem(volume.Name, volume.Format, volume.Source, normalized[index].MountPath, false))
	}
	return normalized, filesystems, nil
}

func (r *AppleContainerRuntime) resolveVolumeForMount(ctx context.Context, mount gateway.VolumeMount) (VolumeConfig, error) {
	candidates := []string{strings.TrimSpace(mount.VolumeID), strings.TrimSpace(mount.Name)}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		volumeName := appleVolumeName(candidate)
		volume, err := r.client.VolumeInspect(ctx, volumeName)
		if err == nil && volume.Name != "" {
			return volume, nil
		}
		if err != nil {
			if errdefs.IsNotFound(err) || isAppleNotFound(err) {
				continue
			}
			return VolumeConfig{}, fmt.Errorf("inspect apple container volume %s: %w", volumeName, err)
		}
	}

	volumes, err := r.client.VolumeList(ctx)
	if err != nil {
		return VolumeConfig{}, fmt.Errorf("list apple container volumes: %w", err)
	}
	for _, volume := range volumes {
		if volume.Labels[appleLocalManagedLabel] != "true" {
			continue
		}
		runtimeVolume := runtimeVolumeFromAppleVolume(volume)
		for _, candidate := range candidates {
			if volumeMatches(runtimeVolume, candidate) {
				return volume, nil
			}
		}
	}

	lookup := firstNonEmpty(mount.VolumeID, mount.Name)
	return VolumeConfig{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, lookup)
}

func normalizeVolumeMounts(mounts []gateway.VolumeMount) []gateway.VolumeMount {
	result := make([]gateway.VolumeMount, 0, len(mounts))
	for _, mount := range mounts {
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
		if volumeID == "" && mountPath == "" {
			continue
		}
		result = append(result, gateway.VolumeMount{
			Name:      name,
			Path:      pathValue,
			VolumeID:  volumeID,
			MountPath: mountPath,
		})
	}
	return result
}

func appleVolumeName(volumeID string) string {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return ""
	}
	if strings.HasPrefix(volumeID, defaultAppleContainerVolumeNamePrefix) {
		return volumeID
	}
	return defaultAppleContainerVolumeNamePrefix + volumeID
}

func runtimeVolumeFromAppleVolume(volume VolumeConfig) gateway.RuntimeVolume {
	volumeID := strings.TrimSpace(volume.Labels[appleLocalVolumeIDLabel])
	if volumeID == "" {
		volumeID = strings.TrimPrefix(strings.TrimSpace(volume.Name), defaultAppleContainerVolumeNamePrefix)
	}
	name := strings.TrimSpace(volume.Labels[appleLocalVolumeNameLabel])
	if name == "" {
		name = volumeID
	}
	return gateway.RuntimeVolume{VolumeID: volumeID, Name: name}
}

func volumeMatches(volume gateway.RuntimeVolume, value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && (value == strings.TrimSpace(volume.VolumeID) || value == strings.TrimSpace(volume.Name))
}

func (r *AppleContainerRuntime) findRuntimeVolume(ctx context.Context, value string) (gateway.RuntimeVolume, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return gateway.RuntimeVolume{}, false, nil
	}
	volumes, err := r.ListVolumes(ctx)
	if err != nil {
		return gateway.RuntimeVolume{}, false, err
	}
	for _, volume := range volumes {
		if volumeMatches(volume, value) {
			return volume, true, nil
		}
	}
	return gateway.RuntimeVolume{}, false, nil
}

func (r *AppleContainerRuntime) containerSnapshot(ctx context.Context, containerID string) (ContainerSnapshot, error) {
	snapshots, err := r.client.ContainerList(ctx, ContainerListFilters{IDs: []string{containerID}})
	if err != nil {
		return ContainerSnapshot{}, err
	}
	if len(snapshots) == 0 {
		return ContainerSnapshot{}, errdefs.NotFound(fmt.Errorf("apple container %s not found", containerID))
	}
	return snapshots[0], nil
}

func (r *AppleContainerRuntime) templateForSnapshot(snapshot ContainerSnapshot) AppleContainerTemplateConfig {
	templateID := strings.TrimSpace(snapshot.Configuration.Labels[appleLocalSandboxTemplateIDLabel])
	if templateID == "" {
		return AppleContainerTemplateConfig{}
	}
	return r.cfg.Templates[templateID]
}

func envdHostPort(ports []PublishPort, envdPort int) string {
	for _, port := range ports {
		if int(port.ContainerPort) == envdPort && strings.EqualFold(strings.TrimSpace(port.Proto), "tcp") {
			return strconv.Itoa(int(port.HostPort))
		}
	}
	return ""
}

func appleEnvdURL(hostPort string) string {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(appleEnvdHost, hostPort)
}

func (r *AppleContainerRuntime) runtimeInfoFromSnapshot(snapshot ContainerSnapshot, info gateway.SandboxRuntimeInfo) gateway.SandboxRuntimeInfo {
	labels := snapshot.Configuration.Labels
	if info.SandboxID == "" {
		info.SandboxID = strings.TrimSpace(labels[appleLocalSandboxIDLabel])
	}
	if info.ContainerID == "" {
		info.ContainerID = snapshot.Configuration.ID
	}
	if info.ContainerName == "" {
		info.ContainerName = snapshot.Configuration.ID
	}
	if info.MachineID == "" {
		info.MachineID = snapshot.Configuration.ID
	}
	if info.HostPort == "" {
		info.HostPort = envdHostPort(snapshot.Configuration.PublishedPorts, r.cfg.EnvdPort)
	}
	if info.EnvdURL == "" && info.HostPort != "" {
		info.EnvdURL = appleEnvdURL(info.HostPort)
	}
	if len(info.VolumeMounts) == 0 {
		info.VolumeMounts = appleVolumeMountsFromLabels(labels)
	}
	return info
}

func e2bStateFromAppleStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return string(e2bapi.Running)
	case "stopped":
		return string(e2bapi.Paused)
	default:
		return ""
	}
}

func stringMapFromLabel(value string) map[string]string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil
	}
	result := make(map[string]string, len(decoded))
	for key, value := range decoded {
		result[key] = value
	}
	return result
}

func appleVolumeMountsFromLabels(labels map[string]string) []gateway.VolumeMount {
	value := strings.TrimSpace(labels[appleLocalSandboxVolumeMountsLabel])
	if value == "" {
		return nil
	}
	var decoded []gateway.VolumeMount
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil
	}
	return normalizeVolumeMounts(decoded)
}

func timeFromLabel(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func boolPtrFromLabel(value string) *bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil
	}
	return &parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (r *AppleContainerRuntime) healthCheck(ctx context.Context, envdURL string) error {
	if r.checkHealthy != nil {
		return r.checkHealthy(ctx, envdURL)
	}
	return r.defaultHealthCheck(ctx, envdURL)
}

func (r *AppleContainerRuntime) defaultHealthCheck(ctx context.Context, envdURL string) error {
	deadline := time.Now().UTC().Add(time.Duration(r.cfg.HealthTimeoutSeconds) * time.Second)
	healthURL := strings.TrimRight(strings.TrimSpace(envdURL), "/") + "/health"
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("wait for envd health %s timed out", envdURL)
		}
		requestCtx, cancel := context.WithTimeout(ctx, remaining)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, healthURL, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("build envd health request: %w", err)
		}
		resp, err := r.httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		cancel()
		if err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().UTC().After(deadline) {
			if err != nil {
				return fmt.Errorf("wait for envd health %s: %w", envdURL, err)
			}
			if resp != nil {
				return fmt.Errorf("wait for envd health %s: unexpected status %d", envdURL, resp.StatusCode)
			}
			return fmt.Errorf("wait for envd health %s timed out", envdURL)
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
