package dockerbackend

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
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
	"strconv"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type DockerRuntimeConfig = gateway.DockerRuntimeConfig
type GatewayTemplate = gateway.GatewayTemplate
type RuntimeVolume = gateway.RuntimeVolume
type SandboxLogsRequest = gateway.SandboxLogsRequest
type SandboxMetricsRequest = gateway.SandboxMetricsRequest
type SandboxPortMapping = gateway.SandboxPortMapping
type SandboxRecord = gateway.SandboxRecord
type SandboxRuntimeCreateRequest = gateway.SandboxRuntimeCreateRequest
type SandboxRuntimeInfo = gateway.SandboxRuntimeInfo
type SandboxRuntimeInspection = gateway.SandboxRuntimeInspection
type SandboxRuntimeLogEntry = gateway.SandboxRuntimeLogEntry
type SandboxRuntimeTemplate = gateway.SandboxRuntimeTemplate
type SnapshotListRequest = gateway.SnapshotListRequest
type TemplateBuildFile = gateway.TemplateBuildFile
type VolumeMount = gateway.VolumeMount

const dockerEnvdPath = "/usr/local/bin/envd"
const dockerEnvdPort = 49983
const dockerEnvdHostIP = "127.0.0.1"
const dockerPublishedHostIP = "0.0.0.0"
const dockerEnvdBinaryAMD64 = "envd-bin/envd-linux-amd64"
const dockerEnvdBinaryARM64 = "envd-bin/envd-linux-arm64"
const defaultSandboxTimeoutSeconds = gateway.DefaultSandboxTimeoutSeconds
const maxSandboxListLimit = gateway.MaxSandboxListLimit

const (
	dockerLocalManagedLabel               = "e2b.local.managed"
	dockerLocalSnapshotLabel              = "e2b.local.snapshot"
	dockerLocalSnapshotSourceSandboxLabel = "e2b.local.snapshot.source_sandbox_id"
	dockerLocalSnapshotNameLabel          = "e2b.local.snapshot.name"
	dockerLocalSnapshotRefLabel           = "e2b.local.snapshot.ref"
	dockerLocalSnapshotCreatedAtLabel     = "e2b.local.snapshot.created_at"
	dockerLocalImageLabel                 = "e2b.local.image"
	dockerLocalSandboxIDLabel             = "e2b.local.sandbox_id"
	dockerLocalSandboxTemplateIDLabel     = "e2b.local.template_id"
	dockerLocalSandboxCreatedAtLabel      = "e2b.local.sandbox.created_at"
	dockerLocalSandboxEndAtLabel          = "e2b.local.sandbox.end_at"
	dockerLocalSandboxMetadataLabel       = "e2b.local.sandbox.metadata"
	dockerLocalSandboxAllowInternetLabel  = "e2b.local.sandbox.allow_internet_access"
	dockerLocalSandboxVolumeMountsLabel   = "e2b.local.sandbox.volume_mounts"
	dockerLocalVolumeMetadataFile         = ".e2b-local-volume.json"
	dockerLocalTemplateLabel              = "e2b.local.template"
	dockerLocalTemplateIDLabel            = "e2b.local.template_id"
	dockerLocalTemplateNamesLabel         = "e2b.local.template.names"
	dockerLocalTemplateBuildIDLabel       = "e2b.local.template.build_id"
	dockerLocalTemplateCPUCountLabel      = "e2b.local.template.cpu_count"
	dockerLocalTemplateMemoryMBLabel      = "e2b.local.template.memory_mb"
	dockerLocalTemplateStartCmdLabel      = "e2b.local.template.start_cmd"
	dockerLocalTemplateReadyCmdLabel      = "e2b.local.template.ready_cmd"
)

type DockerRuntime struct {
	cfg    DockerRuntimeConfig
	client *client.Client
	logger *log.Logger
}

func init() {
	gateway.RegisterSandboxRuntimeFactory("docker", func(cfg gateway.Config, logger *log.Logger) (gateway.SandboxRuntime, error) {
		return NewDockerRuntime(cfg.Docker, logger)
	})
}

func gatewayError(status int, format string, args ...any) gateway.GatewayError {
	return gateway.NewGatewayError(status, format, args...)
}

func gatewayErrorStatus(err error, fallback int) int {
	return gateway.GatewayErrorStatus(err, fallback)
}

func sandboxMetricFromRecord(record SandboxRecord, timestamp time.Time) e2bapi.SandboxMetric {
	return gateway.SandboxMetricFromRecord(record, timestamp)
}

func metricMatchesRange(metric e2bapi.SandboxMetric, req SandboxMetricsRequest) bool {
	return gateway.MetricMatchesRange(metric, req)
}

func sandboxMemoryMB(record SandboxRecord) e2bapi.MemoryMB {
	return gateway.SandboxMemoryMB(record)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	return gateway.AppendUniqueStrings(values, additions...)
}

func stringPtrValue(value *string) string {
	return gateway.StringPtrValue(value)
}

func boolPtrValue(value *bool) bool {
	return gateway.BoolPtrValue(value)
}

func nonEmptyStrings(values []string) []string {
	return gateway.NonEmptyStrings(values)
}

func copyStringMap(values map[string]string) map[string]string {
	return gateway.CopyStringMap(values)
}

func NewDockerRuntime(cfg DockerRuntimeConfig, logger *log.Logger) (*DockerRuntime, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(cfg.Host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	return &DockerRuntime{
		cfg:    cfg,
		client: cli,
		logger: logger,
	}, nil
}

func (r *DockerRuntime) CreateSandbox(ctx context.Context, req SandboxRuntimeCreateRequest) (SandboxRuntimeInfo, error) {
	templateID := strings.TrimSpace(req.TemplateID)
	if templateID == "" {
		return SandboxRuntimeInfo{}, fmt.Errorf("templateID is required")
	}

	imageRef, err := r.resolveTemplateImage(ctx, templateID)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}

	imagePlatform, err := r.imagePlatform(ctx, imageRef)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}
	selectedPlatform := r.selectedPlatform(imagePlatform)
	envdBinary, err := r.envdBinaryForPlatform(selectedPlatform)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}
	if err := validateHostFile(envdBinary); err != nil {
		return SandboxRuntimeInfo{}, err
	}
	imageLabels := r.imageLabels(ctx, imageRef)
	publishedPorts, err := r.publishedContainerPorts(ctx, imageRef)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}

	envdPort := dockerEnvdNatPort()
	exposedPorts := dockerExposedPorts(envdPort, publishedPorts)
	portBindings := dockerPortBindings(envdPort, publishedPorts, r.cfg.PublishedHostIP)
	containerName := r.cfg.ContainerNamePrefix + req.SandboxID
	initEnabled := true
	volumeMounts, mounts, err := r.mounts(ctx, req.VolumeMounts, envdBinary)
	if err != nil {
		return SandboxRuntimeInfo{}, err
	}
	labelReq := req
	labelReq.VolumeMounts = volumeMounts

	resp, err := r.client.ContainerCreate(
		ctx,
		&container.Config{
			Image:        imageRef,
			User:         "root",
			Env:          envVars(req.EnvVars),
			Entrypoint:   []string{dockerEnvdPath},
			Cmd:          r.containerCommand(imageLabels[dockerLocalTemplateStartCmdLabel]),
			ExposedPorts: exposedPorts,
			Labels:       dockerSandboxLabels(labelReq, templateID, imageRef),
		},
		&container.HostConfig{
			Init:         &initEnabled,
			PortBindings: portBindings,
			Mounts:       mounts,
		},
		&network.NetworkingConfig{},
		containerCreatePlatform(selectedPlatform),
		containerName,
	)
	if err != nil {
		return SandboxRuntimeInfo{}, fmt.Errorf("create docker container: %w", err)
	}

	info := SandboxRuntimeInfo{
		SandboxID:     req.SandboxID,
		ContainerID:   resp.ID,
		ContainerName: containerName,
		VolumeMounts:  volumeMounts,
	}

	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = r.removeContainer(context.Background(), resp.ID)
		return SandboxRuntimeInfo{}, fmt.Errorf("start docker container: %w", err)
	}

	info, err = r.inspectRuntimeInfo(ctx, info)
	if err != nil {
		_ = r.removeContainer(context.Background(), resp.ID)
		return SandboxRuntimeInfo{}, err
	}

	if err := r.waitHealthy(ctx, info.EnvdURL); err != nil {
		logs := r.containerLogs(context.Background(), resp.ID)
		_ = r.removeContainer(context.Background(), resp.ID)
		if logs != "" {
			return SandboxRuntimeInfo{}, fmt.Errorf("%w; container logs:\n%s", err, logs)
		}
		return SandboxRuntimeInfo{}, err
	}

	if err := r.waitReadyCommand(ctx, resp.ID, imageLabels[dockerLocalTemplateReadyCmdLabel]); err != nil {
		logs := r.containerLogs(context.Background(), resp.ID)
		_ = r.removeContainer(context.Background(), resp.ID)
		if logs != "" {
			return SandboxRuntimeInfo{}, fmt.Errorf("%w; container logs:\n%s", err, logs)
		}
		return SandboxRuntimeInfo{}, err
	}

	r.logger.Printf("docker sandbox started sandbox_id=%s container_id=%s container_name=%s envd_url=%s",
		req.SandboxID,
		info.ContainerID,
		info.ContainerName,
		info.EnvdURL,
	)

	return info, nil
}

func (r *DockerRuntime) ListTemplates(ctx context.Context) ([]SandboxRuntimeTemplate, error) {
	templatesByID, err := r.listDockerTemplates(ctx)
	if err != nil {
		return nil, err
	}

	templates := make([]SandboxRuntimeTemplate, 0, len(templatesByID))
	for _, template := range templatesByID {
		templates = append(templates, template)
	}

	sort.Slice(templates, func(i, j int) bool {
		return templates[i].TemplateID < templates[j].TemplateID
	})

	return templates, nil
}

func (r *DockerRuntime) DeleteSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	if info.ContainerID == "" {
		return nil
	}

	if err := r.removeContainer(ctx, info.ContainerID); err != nil {
		return err
	}

	r.logger.Printf("docker sandbox removed container_id=%s container_name=%s", info.ContainerID, info.ContainerName)
	return nil
}

func (r *DockerRuntime) PauseSandbox(ctx context.Context, info SandboxRuntimeInfo) error {
	if info.ContainerID == "" {
		return nil
	}

	if err := r.client.ContainerPause(ctx, info.ContainerID); err != nil {
		if errdefs.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("pause docker container: %w", err)
	}

	return nil
}

func (r *DockerRuntime) ResumeSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInfo, error) {
	if info.ContainerID == "" {
		return info, nil
	}

	if err := r.client.ContainerUnpause(ctx, info.ContainerID); err != nil {
		if !errdefs.IsConflict(err) {
			return SandboxRuntimeInfo{}, fmt.Errorf("unpause docker container: %w", err)
		}
	}

	return r.inspectRuntimeInfo(ctx, info)
}

func (r *DockerRuntime) InspectSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInspection, error) {
	containerID := strings.TrimSpace(info.ContainerID)
	if containerID == "" {
		containerID = strings.TrimSpace(info.ContainerName)
	}
	if containerID == "" {
		return SandboxRuntimeInspection{Info: info, Exists: true}, nil
	}

	inspect, err := r.client.ContainerInspect(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return SandboxRuntimeInspection{Exists: false}, nil
		}
		return SandboxRuntimeInspection{}, fmt.Errorf("inspect docker container: %w", err)
	}
	if inspect.ID != "" {
		info.ContainerID = inspect.ID
	}
	if inspect.Name != "" {
		info.ContainerName = strings.TrimPrefix(inspect.Name, "/")
	}
	if runtimeInfo, err := r.inspectRuntimeInfo(ctx, info); err == nil {
		info = runtimeInfo
	}

	state := ""
	if inspect.State != nil {
		switch {
		case inspect.State.Paused:
			state = string(e2bapi.Paused)
		case inspect.State.Running:
			state = string(e2bapi.Running)
		}
	}

	if state == "" {
		return SandboxRuntimeInspection{Info: info, Exists: false}, nil
	}

	return SandboxRuntimeInspection{
		Info:   info,
		State:  state,
		Exists: true,
	}, nil
}

func (r *DockerRuntime) RestoreSandboxes(ctx context.Context) ([]SandboxRecord, error) {
	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", dockerLocalManagedLabel+"=true"),
			filters.Arg("label", dockerLocalSandboxIDLabel),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("list docker sandboxes: %w", err)
	}

	now := time.Now().UTC()
	records := make([]SandboxRecord, 0, len(containers))
	for _, summary := range containers {
		record, ok, err := r.restoreSandboxRecord(ctx, summary, now)
		if err != nil {
			return nil, err
		}
		if ok {
			records = append(records, record)
		}
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

func (r *DockerRuntime) restoreSandboxRecord(ctx context.Context, summary dockertypes.Container, now time.Time) (SandboxRecord, bool, error) {
	inspect, err := r.client.ContainerInspect(ctx, summary.ID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return SandboxRecord{}, false, nil
		}
		return SandboxRecord{}, false, fmt.Errorf("inspect docker sandbox %s: %w", summary.ID, err)
	}

	labels := dockerContainerLabels(summary.Labels, inspect.Config)
	sandboxID := strings.TrimSpace(labels[dockerLocalSandboxIDLabel])
	if sandboxID == "" {
		return SandboxRecord{}, false, nil
	}

	state := dockerSandboxState(inspect.State)
	if state == "" {
		return SandboxRecord{}, false, nil
	}

	info := SandboxRuntimeInfo{
		SandboxID:     sandboxID,
		ContainerID:   inspect.ID,
		ContainerName: strings.TrimPrefix(inspect.Name, "/"),
		VolumeMounts:  dockerVolumeMountsFromLabels(labels),
	}
	if info.ContainerID == "" {
		info.ContainerID = summary.ID
	}
	if info.ContainerName == "" {
		info.ContainerName = firstDockerContainerName(summary.Names)
	}
	if len(info.VolumeMounts) == 0 {
		info.VolumeMounts = dockerVolumeMountsFromMountPoints(inspect.Mounts)
	}
	if runtimeInfo, err := r.inspectRuntimeInfo(ctx, info); err == nil {
		info = runtimeInfo
	} else {
		return SandboxRecord{}, false, err
	}

	createdAt := dockerTimeLabel(labels[dockerLocalSandboxCreatedAtLabel], dockerImageCreatedAt(inspect.Created, now))
	endAt := dockerTimeLabel(labels[dockerLocalSandboxEndAtLabel], now.Add(time.Duration(defaultSandboxTimeoutSeconds)*time.Second))
	templateID := strings.TrimSpace(labels[dockerLocalSandboxTemplateIDLabel])
	if templateID == "" {
		templateID = dockerTemplateName(summary.Image)
	}

	return SandboxRecord{
		ID:                  sandboxID,
		TemplateID:          templateID,
		Metadata:            dockerStringMapLabel(labels[dockerLocalSandboxMetadataLabel]),
		EnvdURL:             info.EnvdURL,
		RuntimeInfo:         info,
		CreatedAt:           createdAt,
		EndAt:               endAt,
		State:               state,
		AllowInternetAccess: dockerBoolPtrLabel(labels[dockerLocalSandboxAllowInternetLabel]),
	}, true, nil
}

func (r *DockerRuntime) GetSandboxLogs(ctx context.Context, info SandboxRuntimeInfo, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error) {
	if info.ContainerID == "" {
		return []SandboxRuntimeLogEntry{}, nil
	}

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
	}
	if tail := dockerLogTail(req); tail > 0 {
		options.Tail = strconv.Itoa(tail)
	}
	if req.Start != nil && *req.Start > 0 {
		options.Since = strconv.FormatInt(*req.Start/1000, 10)
	}
	if req.Cursor != nil && *req.Cursor > 0 {
		options.Since = strconv.FormatInt(*req.Cursor/1000, 10)
	}

	reader, err := r.client.ContainerLogs(ctx, info.ContainerID, options)
	if err != nil {
		return nil, fmt.Errorf("read docker logs: %w", err)
	}
	defer reader.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, reader); err != nil {
		return nil, fmt.Errorf("decode docker logs: %w", err)
	}

	entries := dockerLogEntries(stdout.String(), e2bapi.LogLevelInfo, req)
	entries = append(entries, dockerLogEntries(stderr.String(), e2bapi.LogLevelError, req)...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})
	if req.Direction != nil && *req.Direction == e2bapi.LogsDirectionBackward {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	entries = limitDockerLogEntries(entries, req.Limit)
	return entries, nil
}

func (r *DockerRuntime) BuildTemplate(ctx context.Context, template GatewayTemplate, req e2bapi.TemplateBuildRequest) (GatewayTemplate, []e2bapi.BuildLogEntry, error) {
	dockerfile := strings.TrimSpace(req.Dockerfile)
	if dockerfile == "" {
		return GatewayTemplate{}, nil, fmt.Errorf("dockerfile is required")
	}

	return r.buildTemplateFromDockerfile(ctx, template, dockerTemplateBuildOptions{
		Dockerfile: dockerfile,
		StartCmd:   stringPtrValue(req.StartCmd),
		ReadyCmd:   stringPtrValue(req.ReadyCmd),
	})
}

func (r *DockerRuntime) StartTemplateBuildV2(ctx context.Context, template GatewayTemplate, buildID string, req e2bapi.TemplateBuildStartV2, files []TemplateBuildFile) (GatewayTemplate, []e2bapi.BuildLogEntry, error) {
	dockerfile, err := r.dockerfileFromTemplateBuildStart(ctx, req)
	if err != nil {
		return GatewayTemplate{}, nil, err
	}
	template.BuildID = strings.TrimSpace(buildID)
	return r.buildTemplateFromDockerfile(ctx, template, dockerTemplateBuildOptions{
		Dockerfile: dockerfile,
		Files:      files,
		NoCache:    boolPtrValue(req.Force),
		StartCmd:   stringPtrValue(req.StartCmd),
		ReadyCmd:   stringPtrValue(req.ReadyCmd),
	})
}

type dockerTemplateBuildOptions struct {
	Dockerfile string
	Files      []TemplateBuildFile
	NoCache    bool
	StartCmd   string
	ReadyCmd   string
}

func (r *DockerRuntime) buildTemplateFromDockerfile(ctx context.Context, template GatewayTemplate, opts dockerTemplateBuildOptions) (GatewayTemplate, []e2bapi.BuildLogEntry, error) {
	imageRef := dockerTemplateReference(template)
	buildContext, err := dockerBuildContext(opts.Dockerfile, opts.Files...)
	if err != nil {
		return GatewayTemplate{}, nil, err
	}
	if closer, ok := buildContext.(io.Closer); ok {
		defer closer.Close()
	}
	labels := dockerTemplateLabels(template)
	if strings.TrimSpace(opts.StartCmd) != "" {
		labels[dockerLocalTemplateStartCmdLabel] = strings.TrimSpace(opts.StartCmd)
	}
	if strings.TrimSpace(opts.ReadyCmd) != "" {
		labels[dockerLocalTemplateReadyCmdLabel] = strings.TrimSpace(opts.ReadyCmd)
	}

	resp, err := r.client.ImageBuild(ctx, buildContext, dockertypes.ImageBuildOptions{
		Dockerfile:  "Dockerfile",
		ForceRemove: true,
		Labels:      labels,
		NoCache:     opts.NoCache,
		Platform:    r.cfg.Platform,
		PullParent:  false,
		Remove:      true,
		Tags:        []string{imageRef},
	})
	if err != nil {
		return GatewayTemplate{}, nil, fmt.Errorf("build docker template image: %w", err)
	}
	defer resp.Body.Close()

	logs, err := dockerBuildLogEntries(resp.Body)
	if err != nil {
		return GatewayTemplate{}, logs, err
	}

	inspect, _, err := r.client.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return GatewayTemplate{}, logs, fmt.Errorf("inspect docker template image: %w", err)
	}

	now := time.Now().UTC()
	built := template
	built.ImageRef = imageRef
	built.BuildStatus = e2bapi.TemplateBuildStatusReady
	built.DiskSizeMB = int32(bytesToMB(inspect.Size))
	built.UpdatedAt = now
	if built.BuildID == "" {
		built.BuildID = shortDockerImageID(inspect.ID)
	}
	if built.CreatedAt.IsZero() {
		built.CreatedAt = dockerImageCreatedAt(inspect.Created, now)
	}
	if len(built.Names) == 0 {
		built.Names = []string{built.TemplateID}
	}

	r.logger.Printf("docker template built template_id=%s image_ref=%s image_id=%s build_id=%s",
		built.TemplateID,
		built.ImageRef,
		shortDockerImageID(inspect.ID),
		built.BuildID,
	)

	if len(logs) == 0 {
		logs = []e2bapi.BuildLogEntry{dockerBuildLogEntry(e2bapi.LogLevelInfo, "docker image build completed: "+imageRef)}
	}
	return built, logs, nil
}

func (r *DockerRuntime) GetSandboxMetrics(ctx context.Context, record SandboxRecord, req SandboxMetricsRequest) ([]e2bapi.SandboxMetric, error) {
	if record.RuntimeInfo.ContainerID == "" {
		metric := sandboxMetricFromRecord(record, time.Now().UTC())
		if !metricMatchesRange(metric, req) {
			return []e2bapi.SandboxMetric{}, nil
		}
		return []e2bapi.SandboxMetric{metric}, nil
	}

	reader, err := r.client.ContainerStatsOneShot(ctx, record.RuntimeInfo.ContainerID)
	if err != nil {
		return nil, fmt.Errorf("read docker stats: %w", err)
	}
	defer reader.Body.Close()

	var stats container.StatsResponse
	if err := json.NewDecoder(reader.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decode docker stats: %w", err)
	}

	timestamp := stats.Read
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	metric := sandboxMetricFromRecord(record, timestamp)
	metric.CpuUsedPct = dockerCPUUsedPct(stats)
	metric.MemUsed = uint64ToInt64(stats.MemoryStats.Usage)
	metric.MemTotal = uint64ToInt64(stats.MemoryStats.Limit)
	if metric.MemTotal == 0 {
		metric.MemTotal = int64(sandboxMemoryMB(record)) * 1024 * 1024
	}
	metric.MemCache = uint64ToInt64(dockerMemoryCache(stats))

	if !metricMatchesRange(metric, req) {
		return []e2bapi.SandboxMetric{}, nil
	}
	return []e2bapi.SandboxMetric{metric}, nil
}

func (r *DockerRuntime) CreateSandboxSnapshot(ctx context.Context, record SandboxRecord, req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) (e2bapi.SnapshotInfo, error) {
	if record.RuntimeInfo.ContainerID == "" {
		return e2bapi.SnapshotInfo{}, fmt.Errorf("sandbox %s has no docker container", record.ID)
	}

	name := snapshotRequestName(req)
	createdAt := time.Now().UTC()
	ref := dockerSnapshotReference(record.ID, name, createdAt)
	changes := []string{
		"LABEL " + dockerLocalManagedLabel + "=true",
		"LABEL " + dockerLocalSnapshotLabel + "=true",
		"LABEL " + dockerLocalSnapshotSourceSandboxLabel + "=" + record.ID,
		"LABEL " + dockerLocalSnapshotRefLabel + "=" + ref,
		"LABEL " + dockerLocalSnapshotCreatedAtLabel + "=" + strconv.FormatInt(createdAt.Unix(), 10),
	}
	if name != "" {
		changes = append(changes, "LABEL "+dockerLocalSnapshotNameLabel+"="+name)
	}

	if _, err := r.client.ContainerCommit(ctx, record.RuntimeInfo.ContainerID, container.CommitOptions{
		Reference: ref,
		Comment:   "e2b-local snapshot for sandbox " + record.ID,
		Author:    "e2b-local",
		Changes:   changes,
		Pause:     true,
	}); err != nil {
		return e2bapi.SnapshotInfo{}, fmt.Errorf("commit docker snapshot: %w", err)
	}

	return e2bapi.SnapshotInfo{
		SnapshotID: ref,
		Names:      []string{ref},
	}, nil
}

func (r *DockerRuntime) ListSnapshots(ctx context.Context, req SnapshotListRequest) ([]e2bapi.SnapshotInfo, error) {
	args := filters.NewArgs(filters.Arg("label", dockerLocalSnapshotLabel+"=true"))
	if strings.TrimSpace(req.SandboxID) != "" {
		args.Add("label", dockerLocalSnapshotSourceSandboxLabel+"="+strings.TrimSpace(req.SandboxID))
	}

	images, err := r.client.ImageList(ctx, image.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		return nil, fmt.Errorf("list docker snapshots: %w", err)
	}

	summariesByID := make(map[string]image.Summary)
	for _, img := range images {
		info := snapshotInfoFromDockerImage(img)
		if info.SnapshotID == "" {
			continue
		}
		if existing, ok := summariesByID[info.SnapshotID]; !ok || img.Created > existing.Created {
			summariesByID[info.SnapshotID] = img
		}
	}

	summaries := make([]image.Summary, 0, len(summariesByID))
	for _, img := range summariesByID {
		summaries = append(summaries, img)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Created == summaries[j].Created {
			return snapshotInfoFromDockerImage(summaries[i]).SnapshotID < snapshotInfoFromDockerImage(summaries[j]).SnapshotID
		}
		return summaries[i].Created > summaries[j].Created
	})

	start := 0
	nextToken := strings.TrimSpace(req.NextToken)
	if nextToken != "" {
		start = len(summaries)
		for i, img := range summaries {
			if snapshotInfoFromDockerImage(img).SnapshotID == nextToken {
				start = i + 1
				break
			}
		}
	}

	if start > len(summaries) {
		start = len(summaries)
	}

	limit := req.Limit
	if limit <= 0 || limit > maxSandboxListLimit {
		limit = maxSandboxListLimit
	}

	end := start + limit
	if end > len(summaries) {
		end = len(summaries)
	}

	snapshots := make([]e2bapi.SnapshotInfo, 0, end-start)
	for _, img := range summaries[start:end] {
		snapshots = append(snapshots, snapshotInfoFromDockerImage(img))
	}
	return snapshots, nil
}

func validateHostFile(envdBinary string) error {
	stat, err := os.Stat(envdBinary)
	if err != nil {
		return fmt.Errorf("docker.envd_binary is not accessible: %w", err)
	}

	if stat.IsDir() {
		return fmt.Errorf("docker.envd_binary is a directory: %s", envdBinary)
	}

	if stat.Mode()&0o111 == 0 {
		return fmt.Errorf("docker.envd_binary is not executable: %s", envdBinary)
	}

	return nil
}

func (r *DockerRuntime) resolveTemplateImage(ctx context.Context, templateID string) (string, error) {
	imageRefs, err := r.templateImageRefs(ctx)
	if err != nil {
		return "", err
	}

	if imageRef, ok := imageRefs[templateID]; ok {
		return imageRef, nil
	}

	if isDockerImageReference(templateID) {
		if _, _, err := r.client.ImageInspectWithRaw(ctx, templateID); err != nil {
			if errdefs.IsNotFound(err) {
				return "", fmt.Errorf("docker image %q is not available locally; pull or build it before creating a sandbox", templateID)
			}
			return "", fmt.Errorf("inspect docker image %q: %w", templateID, err)
		}
		return templateID, nil
	}

	return "", fmt.Errorf("templateID must be a listed docker template or locally available docker image reference with tag or digest")
}

func (r *DockerRuntime) templateImageRefs(ctx context.Context) (map[string]string, error) {
	templatesByID, err := r.listDockerTemplates(ctx)
	if err != nil {
		return nil, err
	}

	imageRefs := make(map[string]string, len(templatesByID)*2)
	for _, template := range templatesByID {
		if template.ImageRef == "" {
			continue
		}
		imageRefs[template.TemplateID] = template.ImageRef
		imageRefs[template.ImageRef] = template.ImageRef
		imageRefs[shortDockerImageName(template.ImageRef)] = template.ImageRef
	}

	return imageRefs, nil
}

func (r *DockerRuntime) listDockerTemplates(ctx context.Context) (map[string]SandboxRuntimeTemplate, error) {
	images, err := r.client.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list docker images: %w", err)
	}

	refsByImageRef := make(map[string]dockerImageRefSummary)
	for _, img := range images {
		if img.Labels[dockerLocalSnapshotLabel] == "true" {
			continue
		}
		imageRefs := taggedImageRefs(img.RepoTags)
		if len(imageRefs) == 0 {
			continue
		}
		for _, imageRef := range imageRefs {
			refsByImageRef[imageRef] = dockerImageRefSummary{imageRef: imageRef, image: img}
		}
	}
	refs := make([]dockerImageRefSummary, 0, len(refsByImageRef))
	for _, ref := range refsByImageRef {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].imageRef < refs[j].imageRef
	})

	templateIDCounts := make(map[string]int, len(refs))
	for _, ref := range refs {
		templateIDCounts[dockerTemplateName(ref.imageRef)]++
	}

	templatesByID := make(map[string]SandboxRuntimeTemplate)
	for _, ref := range refs {
		templateID := dockerTemplateName(ref.imageRef)
		if templateIDCounts[templateID] > 1 {
			templateID = shortDockerImageName(ref.imageRef)
		}

		if _, exists := templatesByID[templateID]; exists {
			continue
		}

		templatesByID[templateID] = templateFromDockerImage(templateID, ref.imageRef, ref.image)
	}

	return templatesByID, nil
}

func isDockerImageReference(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if strings.Contains(ref, "@") {
		return true
	}
	lastSlash := strings.LastIndex(ref, "/")
	return strings.Contains(ref[lastSlash+1:], ":")
}

type dockerImageRefSummary struct {
	imageRef string
	image    image.Summary
}

func taggedImageRefs(repoTags []string) []string {
	refs := make([]string, 0, len(repoTags))
	for _, tag := range repoTags {
		tag = strings.TrimSpace(tag)
		if tag == "" || tag == "<none>:<none>" || !isDockerImageReference(tag) {
			continue
		}
		refs = append(refs, tag)
	}
	sort.Strings(refs)
	return refs
}

func shortDockerImageName(imageRef string) string {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return ""
	}

	lastSlash := strings.LastIndex(imageRef, "/")
	return imageRef[lastSlash+1:]
}

func dockerTemplateName(imageRef string) string {
	name := shortDockerImageName(imageRef)
	if digestIndex := strings.Index(name, "@"); digestIndex >= 0 {
		return name[:digestIndex]
	}
	if tagIndex := strings.LastIndex(name, ":"); tagIndex >= 0 {
		return name[:tagIndex]
	}
	return name
}

func snapshotRequestName(req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) string {
	if req.Name == nil {
		return ""
	}
	return dockerSafeImageComponent(*req.Name)
}

func dockerSnapshotReference(sandboxID string, name string, createdAt time.Time) string {
	component := dockerSafeImageComponent(name)
	if component == "" {
		component = dockerSafeImageComponent(sandboxID + "-" + strconv.FormatInt(createdAt.Unix(), 10))
	}
	if component == "" {
		component = "snapshot-" + strconv.FormatInt(createdAt.Unix(), 10)
	}
	return "e2b-local/snapshots/" + component + ":default"
}

func dockerTemplateReference(template GatewayTemplate) string {
	component := dockerSafeImageComponent(template.TemplateID)
	if component == "" {
		component = dockerSafeImageComponent(strings.Join(template.Names, "-"))
	}
	if component == "" {
		component = "template"
	}
	return "e2b-local/templates/" + component + ":latest"
}

func (r *DockerRuntime) dockerfileFromTemplateBuildStart(ctx context.Context, req e2bapi.TemplateBuildStartV2) (string, error) {
	fromImage := stringPtrValue(req.FromImage)
	fromTemplate := stringPtrValue(req.FromTemplate)
	if fromImage != "" && fromTemplate != "" {
		return "", gatewayError(http.StatusBadRequest, "cannot specify both fromImage and fromTemplate")
	}
	if fromTemplate != "" {
		imageRef, err := r.resolveTemplateImage(ctx, fromTemplate)
		if err != nil {
			return "", gatewayError(http.StatusBadRequest, "base template %s not found: %s", fromTemplate, err.Error())
		}
		fromImage = imageRef
	}
	if fromImage == "" {
		return "", gatewayError(http.StatusBadRequest, "must specify either fromImage or fromTemplate")
	}

	var dockerfile strings.Builder
	dockerfile.WriteString("FROM ")
	dockerfile.WriteString(fromImage)
	dockerfile.WriteByte('\n')

	for index, step := range templateBuildSteps(req.Steps) {
		line, err := dockerfileInstructionFromTemplateStep(step)
		if err != nil {
			return "", gatewayError(gatewayErrorStatus(err, http.StatusBadRequest), "template step %d: %s", index, err.Error())
		}
		if line == "" {
			continue
		}
		dockerfile.WriteString(line)
		dockerfile.WriteByte('\n')
	}

	return dockerfile.String(), nil
}

func templateBuildSteps(steps *[]e2bapi.TemplateStep) []e2bapi.TemplateStep {
	if steps == nil {
		return nil
	}
	return append([]e2bapi.TemplateStep(nil), (*steps)...)
}

func dockerfileInstructionFromTemplateStep(step e2bapi.TemplateStep) (string, error) {
	stepType := strings.ToUpper(strings.TrimSpace(step.Type))
	args := templateStepArgs(step)
	switch stepType {
	case "":
		return "", fmt.Errorf("type is required")
	case "RUN":
		if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("RUN requires command argument")
		}
		if len(args) >= 2 && strings.TrimSpace(args[1]) != "" {
			return "USER " + strings.TrimSpace(args[1]) + "\nRUN " + args[0], nil
		}
		return "RUN " + args[0], nil
	case "COPY":
		if len(args) < 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
			return "", fmt.Errorf("COPY requires local path and container path arguments")
		}
		if strings.TrimSpace(stringPtrValue(step.FilesHash)) == "" {
			return "", fmt.Errorf("COPY requires filesHash")
		}
		options := []string{}
		if len(args) >= 3 && strings.TrimSpace(args[2]) != "" {
			options = append(options, "--chown="+strings.TrimSpace(args[2]))
		}
		if len(args) >= 4 && strings.TrimSpace(args[3]) != "" {
			options = append(options, "--chmod="+strings.TrimSpace(args[3]))
		}
		parts := append([]string{"COPY"}, options...)
		parts = append(parts, args[0], args[1])
		return strings.Join(parts, " "), nil
	case "ENV":
		if len(args) == 0 {
			return "", fmt.Errorf("ENV requires key/value arguments")
		}
		if len(args)%2 != 0 {
			return "", fmt.Errorf("ENV requires both key and value arguments")
		}
		pairs := make([]string, 0, len(args)/2)
		for i := 0; i < len(args)-1; i += 2 {
			key := strings.TrimSpace(args[i])
			if key == "" {
				return "", fmt.Errorf("ENV key cannot be empty")
			}
			pairs = append(pairs, key+"="+strconv.Quote(args[i+1]))
		}
		return "ENV " + strings.Join(pairs, " "), nil
	case "WORKDIR":
		if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("WORKDIR requires path argument")
		}
		return "WORKDIR " + args[0], nil
	case "USER":
		if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("USER requires username argument")
		}
		return "USER " + strings.TrimSpace(args[0]), nil
	default:
		if len(args) == 0 {
			return "", fmt.Errorf("%s requires arguments", stepType)
		}
		return stepType + " " + strings.Join(args, " "), nil
	}
}

func templateStepArgs(step e2bapi.TemplateStep) []string {
	if step.Args == nil {
		return nil
	}
	return append([]string(nil), (*step.Args)...)
}

func dockerBuildContext(dockerfile string, files ...TemplateBuildFile) (io.Reader, error) {
	reader, writer := io.Pipe()
	go func() {
		tarWriter := tar.NewWriter(writer)
		err := writeDockerBuildContext(tarWriter, dockerfile, files...)
		if closeErr := tarWriter.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return reader, nil
}

func writeDockerBuildContext(writer *tar.Writer, dockerfile string, files ...TemplateBuildFile) error {
	data := []byte(dockerfile)
	if err := writer.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		return fmt.Errorf("write docker build context header: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("write dockerfile to build context: %w", err)
	}
	for _, file := range files {
		if err := appendTemplateFileToDockerBuildContext(writer, file); err != nil {
			return err
		}
	}
	return nil
}

func appendTemplateFileToDockerBuildContext(writer *tar.Writer, file TemplateBuildFile) error {
	archiveReader, err := file.Open()
	if err != nil {
		return fmt.Errorf("open uploaded template file archive %s: %w", file.Hash, err)
	}
	defer archiveReader.Close()

	gzipReader, err := gzip.NewReader(archiveReader)
	if err != nil {
		return fmt.Errorf("open uploaded template file archive %s: %w", file.Hash, err)
	}
	defer gzipReader.Close()

	reader := tar.NewReader(gzipReader)
	entryCount := 0
	var uncompressedBytes int64
	for {
		header, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read uploaded template file archive %s: %w", file.Hash, err)
		}

		entryCount++
		if entryCount > gateway.MaxTemplateArchiveEntries() {
			return fmt.Errorf("uploaded template file archive %s contains more than %d entries", file.Hash, gateway.MaxTemplateArchiveEntries())
		}
		name, ok := safeBuildContextTarName(header.Name)
		if !ok {
			return fmt.Errorf("uploaded template file archive %s contains unsafe path %q", file.Hash, header.Name)
		}

		copiedHeader := *header
		copiedHeader.Name = name
		if copiedHeader.Linkname != "" {
			linkName, ok := safeBuildContextTarName(copiedHeader.Linkname)
			if !ok {
				return fmt.Errorf("uploaded template file archive %s contains unsafe link path %q", file.Hash, copiedHeader.Linkname)
			}
			copiedHeader.Linkname = linkName
		}
		if copiedHeader.Typeflag == tar.TypeReg || copiedHeader.Typeflag == tar.TypeRegA {
			if copiedHeader.Size < 0 {
				return fmt.Errorf("uploaded template file archive %s contains invalid size for %q", file.Hash, copiedHeader.Name)
			}
			if copiedHeader.Size > gateway.MaxTemplateArchiveUncompressedBytes()-uncompressedBytes {
				return fmt.Errorf("uploaded template file archive %s exceeds %d uncompressed bytes", file.Hash, gateway.MaxTemplateArchiveUncompressedBytes())
			}
		}
		if err := writer.WriteHeader(&copiedHeader); err != nil {
			return fmt.Errorf("write uploaded template file %s to build context: %w", copiedHeader.Name, err)
		}
		if copiedHeader.Typeflag == tar.TypeReg || copiedHeader.Typeflag == tar.TypeRegA {
			n, err := io.Copy(writer, reader)
			if err != nil {
				return fmt.Errorf("copy uploaded template file %s to build context: %w", copiedHeader.Name, err)
			}
			uncompressedBytes += n
			if uncompressedBytes > gateway.MaxTemplateArchiveUncompressedBytes() {
				return fmt.Errorf("uploaded template file archive %s exceeds %d uncompressed bytes", file.Hash, gateway.MaxTemplateArchiveUncompressedBytes())
			}
		}
	}
}

func safeBuildContextTarName(name string) (string, bool) {
	return gateway.SafeTemplateBuildContextTarName(name)
}

func dockerTemplateLabels(template GatewayTemplate) map[string]string {
	labels := map[string]string{
		dockerLocalManagedLabel:          "true",
		dockerLocalTemplateLabel:         "true",
		dockerLocalTemplateIDLabel:       template.TemplateID,
		dockerLocalTemplateNamesLabel:    strings.Join(appendUniqueStrings(template.Names, template.TemplateID), ","),
		dockerLocalTemplateBuildIDLabel:  template.BuildID,
		dockerLocalTemplateCPUCountLabel: strconv.Itoa(int(template.CpuCount)),
		dockerLocalTemplateMemoryMBLabel: strconv.Itoa(int(template.MemoryMB)),
	}
	for key, value := range labels {
		if strings.TrimSpace(value) == "" {
			delete(labels, key)
		}
	}
	return labels
}

func dockerSandboxLabels(req SandboxRuntimeCreateRequest, templateID string, imageRef string) map[string]string {
	labels := map[string]string{
		dockerLocalManagedLabel:           "true",
		dockerLocalImageLabel:             imageRef,
		dockerLocalSandboxIDLabel:         req.SandboxID,
		dockerLocalSandboxTemplateIDLabel: templateID,
	}
	if !req.CreatedAt.IsZero() {
		labels[dockerLocalSandboxCreatedAtLabel] = req.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if !req.EndAt.IsZero() {
		labels[dockerLocalSandboxEndAtLabel] = req.EndAt.UTC().Format(time.RFC3339Nano)
	}
	if len(req.Metadata) > 0 {
		labels[dockerLocalSandboxMetadataLabel] = dockerJSONLabel(req.Metadata)
	}
	if len(req.VolumeMounts) > 0 {
		labels[dockerLocalSandboxVolumeMountsLabel] = dockerJSONLabel(req.VolumeMounts)
	}
	if req.AllowInternetAccess != nil {
		labels[dockerLocalSandboxAllowInternetLabel] = strconv.FormatBool(*req.AllowInternetAccess)
	}
	for key, value := range labels {
		if strings.TrimSpace(value) == "" {
			delete(labels, key)
		}
	}
	return labels
}

func dockerJSONLabel(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func dockerContainerLabels(summaryLabels map[string]string, config *container.Config) map[string]string {
	labels := map[string]string{}
	for key, value := range summaryLabels {
		labels[key] = value
	}
	if config != nil {
		for key, value := range config.Labels {
			labels[key] = value
		}
	}
	return labels
}

func dockerTimeLabel(value string, fallback time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.UTC()
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fallback.UTC()
	}
	return parsed.UTC()
}

func dockerStringMapLabel(value string) map[string]string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil
	}
	return copyStringMap(decoded)
}

func dockerBoolPtrLabel(value string) *bool {
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

func dockerVolumeMountsFromLabels(labels map[string]string) []VolumeMount {
	value := strings.TrimSpace(labels[dockerLocalSandboxVolumeMountsLabel])
	if value == "" {
		return nil
	}
	var decoded []VolumeMount
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil
	}
	return normalizeVolumeMounts(decoded)
}

func dockerVolumeMountsFromMountPoints(mounts []dockertypes.MountPoint) []VolumeMount {
	result := make([]VolumeMount, 0, len(mounts))
	for _, mountPoint := range mounts {
		if mountPoint.Type != mount.TypeVolume {
			continue
		}
		name := strings.TrimSpace(mountPoint.Name)
		path := strings.TrimSpace(mountPoint.Destination)
		if name == "" || path == "" {
			continue
		}
		result = append(result, VolumeMount{
			Name:      name,
			Path:      path,
			VolumeID:  name,
			MountPath: path,
		})
	}
	return result
}

func firstDockerContainerName(names []string) string {
	for _, name := range names {
		name = strings.Trim(strings.TrimSpace(name), "/")
		if name != "" {
			return name
		}
	}
	return ""
}

func dockerSandboxState(state *dockertypes.ContainerState) string {
	if state == nil {
		return ""
	}
	switch {
	case state.Paused:
		return string(e2bapi.Paused)
	case state.Running:
		return string(e2bapi.Running)
	default:
		return ""
	}
}

type dockerBuildStreamMessage struct {
	Stream      string `json:"stream"`
	Status      string `json:"status"`
	Progress    string `json:"progress"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func dockerBuildLogEntries(reader io.Reader) ([]e2bapi.BuildLogEntry, error) {
	decoder := json.NewDecoder(reader)
	logs := []e2bapi.BuildLogEntry{}
	for {
		var message dockerBuildStreamMessage
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return logs, nil
			}
			return logs, fmt.Errorf("decode docker build output: %w", err)
		}

		if errMessage := dockerBuildErrorMessage(message); errMessage != "" {
			logs = append(logs, dockerBuildLogEntry(e2bapi.LogLevelError, errMessage))
			return logs, fmt.Errorf("docker template build failed: %s", errMessage)
		}

		for _, line := range dockerBuildMessageLines(message) {
			logs = append(logs, dockerBuildLogEntry(e2bapi.LogLevelInfo, line))
		}
	}
}

func dockerBuildErrorMessage(message dockerBuildStreamMessage) string {
	if message.ErrorDetail != nil && strings.TrimSpace(message.ErrorDetail.Message) != "" {
		return strings.TrimSpace(message.ErrorDetail.Message)
	}
	return strings.TrimSpace(message.Error)
}

func dockerBuildMessageLines(message dockerBuildStreamMessage) []string {
	text := message.Stream
	if text == "" {
		text = message.Status
		if message.Progress != "" {
			text += " " + message.Progress
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")

	lines := strings.Split(text, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func dockerBuildLogEntry(level e2bapi.LogLevel, message string) e2bapi.BuildLogEntry {
	return e2bapi.BuildLogEntry{
		Level:     level,
		Message:   message,
		Timestamp: time.Now().UTC(),
	}
}

func dockerSafeImageComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	var b strings.Builder
	lastDash := false
	for _, r := range value {
		isAlphaNumeric := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNumeric {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() == 0 || lastDash {
			continue
		}
		b.WriteByte('-')
		lastDash = true
	}

	result := strings.Trim(b.String(), "-")
	if len(result) > 96 {
		result = strings.Trim(result[:96], "-")
	}
	return result
}

func snapshotInfoFromDockerImage(img image.Summary) e2bapi.SnapshotInfo {
	names := taggedImageRefs(img.RepoTags)
	snapshotID := ""
	if img.Labels != nil {
		snapshotID = strings.TrimSpace(img.Labels[dockerLocalSnapshotRefLabel])
	}
	if snapshotID == "" && len(names) > 0 {
		snapshotID = names[0]
	}
	if snapshotID == "" {
		snapshotID = shortDockerImageID(img.ID)
	}
	if len(names) == 0 && snapshotID != "" {
		names = []string{snapshotID}
	}
	return e2bapi.SnapshotInfo{
		Names:      names,
		SnapshotID: snapshotID,
	}
}

func templateFromDockerImage(templateID string, imageRef string, img image.Summary) SandboxRuntimeTemplate {
	createdAt := time.Now().UTC()
	if img.Created > 0 {
		createdAt = time.Unix(img.Created, 0).UTC()
	}

	containers := img.Containers
	if containers < 0 {
		containers = 0
	}

	labels := img.Labels
	if labelTemplateID := strings.TrimSpace(labels[dockerLocalTemplateIDLabel]); labelTemplateID != "" {
		templateID = labelTemplateID
	}
	names := appendUniqueStrings([]string{templateID}, splitDockerLabelList(labels[dockerLocalTemplateNamesLabel])...)
	buildID := shortDockerImageID(img.ID)
	if labelBuildID := strings.TrimSpace(labels[dockerLocalTemplateBuildIDLabel]); labelBuildID != "" {
		buildID = labelBuildID
	}
	cpuCount := dockerLabelInt(labels[dockerLocalTemplateCPUCountLabel], 1)
	memoryMB := dockerLabelInt(labels[dockerLocalTemplateMemoryMBLabel], 512)

	return SandboxRuntimeTemplate{
		TemplateID:  templateID,
		Names:       names,
		ImageRef:    imageRef,
		BuildCount:  1,
		BuildID:     buildID,
		BuildStatus: "ready",
		CPUCount:    cpuCount,
		DiskSizeMB:  bytesToMB(img.Size),
		MemoryMB:    memoryMB,
		Public:      false,
		SpawnCount:  containers,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

func shortDockerImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func bytesToMB(size int64) int {
	if size <= 0 {
		return 0
	}
	return int((size + 1024*1024 - 1) / (1024 * 1024))
}

func dockerImageCreatedAt(value string, fallback time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	createdAt, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fallback
	}
	return createdAt.UTC()
}

func splitDockerLabelList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func dockerLabelInt(value string, fallback int) int {
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

func (r *DockerRuntime) imagePlatform(ctx context.Context, imageRef string) (ocispec.Platform, error) {
	inspect, _, err := r.client.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return ocispec.Platform{}, fmt.Errorf("inspect docker image platform: %w", err)
	}
	return dockerImagePlatform(inspect), nil
}

func dockerImagePlatform(inspect dockertypes.ImageInspect) ocispec.Platform {
	return ocispec.Platform{
		OS:           strings.TrimSpace(inspect.Os),
		Architecture: normalizeImageArchitecture(inspect.Architecture),
		Variant:      strings.TrimSpace(inspect.Variant),
	}
}

func (r *DockerRuntime) envdBinaryForPlatform(platform ocispec.Platform) (string, error) {
	if override := strings.TrimSpace(r.cfg.EnvdBinary); override != "" {
		return override, nil
	}

	if platform.OS != "" && platform.OS != "linux" {
		return "", fmt.Errorf("docker image OS %q is not supported; envd binaries are Linux-only", platform.OS)
	}

	switch normalizeImageArchitecture(platform.Architecture) {
	case "amd64":
		return bundledDockerEnvdBinaryPath(dockerEnvdBinaryAMD64), nil
	case "arm64":
		return bundledDockerEnvdBinaryPath(dockerEnvdBinaryARM64), nil
	case "":
		return "", fmt.Errorf("docker image architecture is unknown; set docker.envd_binary explicitly")
	default:
		return "", fmt.Errorf("docker image architecture %q is not supported; set docker.envd_binary explicitly", platform.Architecture)
	}
}

func normalizeImageArchitecture(architecture string) string {
	switch strings.TrimSpace(architecture) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.TrimSpace(architecture)
	}
}

func (r *DockerRuntime) selectedPlatform(imagePlatform ocispec.Platform) ocispec.Platform {
	if configured := parsePlatform(r.cfg.Platform); configured != nil {
		return *configured
	}
	return imagePlatform
}

func containerCreatePlatform(imagePlatform ocispec.Platform) *ocispec.Platform {
	if strings.TrimSpace(imagePlatform.OS) == "" || strings.TrimSpace(imagePlatform.Architecture) == "" {
		return nil
	}
	platform := imagePlatform
	return &platform
}

func bundledDockerEnvdBinaryPath(relPath string) string {
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

func (r *DockerRuntime) imageLabels(ctx context.Context, imageRef string) map[string]string {
	inspect, _, err := r.client.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		r.logger.Printf("docker image label inspect failed image_ref=%s error=%v", imageRef, err)
		return map[string]string{}
	}
	if inspect.Config == nil || inspect.Config.Labels == nil {
		return map[string]string{}
	}

	labels := make(map[string]string, len(inspect.Config.Labels))
	for key, value := range inspect.Config.Labels {
		labels[key] = value
	}
	return labels
}

func (r *DockerRuntime) publishedContainerPorts(ctx context.Context, imageRef string) ([]int, error) {
	inspect, _, err := r.client.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("inspect docker image exposed ports: %w", err)
	}

	ports := append([]int(nil), r.cfg.PublishedPorts...)
	if inspect.Config != nil {
		ports = append(ports, dockerTCPPortsFromSet(inspect.Config.ExposedPorts)...)
	}
	return normalizeDockerPublishedPorts(ports), nil
}

func normalizeDockerPublishedPorts(ports []int) []int {
	seen := map[int]struct{}{}
	result := make([]int, 0, len(ports))
	for _, port := range ports {
		if port <= 0 || port > 65535 || port == dockerEnvdPort {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		result = append(result, port)
	}
	sort.Ints(result)
	return result
}

func dockerTCPPortsFromSet(portSet nat.PortSet) []int {
	ports := make([]int, 0, len(portSet))
	for port := range portSet {
		containerPort, protocol, ok := parseDockerNatPort(port)
		if !ok || protocol != "tcp" {
			continue
		}
		ports = append(ports, containerPort)
	}
	return ports
}

func parseDockerNatPort(port nat.Port) (int, string, bool) {
	parts := strings.SplitN(string(port), "/", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	containerPort, err := strconv.Atoi(parts[0])
	if err != nil || containerPort <= 0 || containerPort > 65535 {
		return 0, "", false
	}
	protocol := strings.ToLower(strings.TrimSpace(parts[1]))
	if protocol == "" {
		return 0, "", false
	}
	return containerPort, protocol, true
}

func dockerExposedPorts(envdPort nat.Port, publishedPorts []int) nat.PortSet {
	portSet := nat.PortSet{envdPort: struct{}{}}
	for _, port := range publishedPorts {
		portSet[dockerTCPNatPort(port)] = struct{}{}
	}
	return portSet
}

func dockerPortBindings(envdPort nat.Port, publishedPorts []int, publishedHostIP string) nat.PortMap {
	portMap := nat.PortMap{
		envdPort: []nat.PortBinding{{
			HostIP: dockerEnvdHostIP,
		}},
	}
	hostIP := strings.TrimSpace(publishedHostIP)
	if hostIP == "" {
		hostIP = dockerPublishedHostIP
	}
	for _, port := range publishedPorts {
		portMap[dockerTCPNatPort(port)] = []nat.PortBinding{{
			HostIP: hostIP,
		}}
	}
	return portMap
}

func dockerTCPNatPort(port int) nat.Port {
	return nat.Port(fmt.Sprintf("%d/tcp", port))
}

func (r *DockerRuntime) containerCommand(startCmd string) []string {
	cmd := []string{"-isnotfc", "-port", fmt.Sprintf("%d", dockerEnvdPort)}
	if strings.TrimSpace(startCmd) != "" {
		cmd = append(cmd, "-cmd", strings.TrimSpace(startCmd))
	}
	return cmd
}

func dockerReadyCommand(readyCmd string) []string {
	return []string{"/bin/sh", "-lc", strings.TrimSpace(readyCmd)}
}

func (r *DockerRuntime) mounts(ctx context.Context, volumeMounts []VolumeMount, envdBinary string) ([]VolumeMount, []mount.Mount, error) {
	mounts := []mount.Mount{
		{
			Type:     mount.TypeBind,
			Source:   envdBinary,
			Target:   dockerEnvdPath,
			ReadOnly: true,
		},
	}

	normalized := normalizeVolumeMounts(volumeMounts)
	for index, volumeMount := range normalized {
		if volumeMount.VolumeID == "" {
			return nil, nil, fmt.Errorf("volume mount name is required for path %s", volumeMount.Path)
		}
		if volumeMount.Path == "" {
			return nil, nil, fmt.Errorf("volume mount path is required for volume %s", volumeMount.VolumeID)
		}
		if !strings.HasPrefix(volumeMount.Path, "/") {
			return nil, nil, fmt.Errorf("volume mount path must be absolute: %s", volumeMount.Path)
		}
		resolved, hostDir, err := r.ensureLocalVolume(volumeMount.VolumeID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil, nil, fmt.Errorf("volume %s not found", volumeMount.VolumeID)
			}
			return nil, nil, err
		}
		normalized[index].VolumeID = resolved.VolumeID
		normalized[index].Name = resolved.Name
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: hostDir,
			Target: volumeMount.Path,
		})
	}

	return normalized, mounts, nil
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

func (r *DockerRuntime) inspectRuntimeInfo(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInfo, error) {
	inspect, err := r.client.ContainerInspect(ctx, info.ContainerID)
	if err != nil {
		return SandboxRuntimeInfo{}, fmt.Errorf("inspect docker container: %w", err)
	}

	envdPort := dockerEnvdNatPort()
	bindings := inspect.NetworkSettings.Ports[envdPort]
	if len(bindings) == 0 {
		return SandboxRuntimeInfo{}, fmt.Errorf("docker container has no host binding for %s", envdPort)
	}

	info.HostPort = bindings[0].HostPort
	info.EnvdURL = fmt.Sprintf("http://%s:%s", dockerEnvdHost(bindings[0]), info.HostPort)
	info.ContainerIP = dockerContainerIP(inspect.NetworkSettings)
	info.PublishedPorts = dockerPublishedPortsFromBindings(inspect.NetworkSettings.Ports)
	if info.ContainerName == "" {
		info.ContainerName = strings.TrimPrefix(inspect.Name, "/")
	}

	return info, nil
}

func dockerEnvdNatPort() nat.Port {
	return nat.Port(fmt.Sprintf("%d/tcp", dockerEnvdPort))
}

func dockerPublishedPortsFromBindings(ports nat.PortMap) []SandboxPortMapping {
	if len(ports) == 0 {
		return nil
	}
	result := []SandboxPortMapping{}
	for port, bindings := range ports {
		containerPort, protocol, ok := parseDockerNatPort(port)
		if !ok || containerPort == dockerEnvdPort {
			continue
		}
		for _, binding := range bindings {
			hostPort, err := strconv.Atoi(strings.TrimSpace(binding.HostPort))
			if err != nil || hostPort <= 0 {
				continue
			}
			result = append(result, SandboxPortMapping{
				ContainerPort: containerPort,
				HostIP:        strings.TrimSpace(binding.HostIP),
				HostPort:      hostPort,
				Protocol:      protocol,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ContainerPort != result[j].ContainerPort {
			return result[i].ContainerPort < result[j].ContainerPort
		}
		if result[i].Protocol != result[j].Protocol {
			return result[i].Protocol < result[j].Protocol
		}
		return result[i].HostPort < result[j].HostPort
	})
	return result
}

func dockerEnvdHost(binding nat.PortBinding) string {
	hostIP := strings.TrimSpace(binding.HostIP)
	switch hostIP {
	case "", "0.0.0.0", "::":
		return dockerEnvdHostIP
	default:
		return hostIP
	}
}

func dockerContainerIP(settings *dockertypes.NetworkSettings) string {
	if settings == nil {
		return ""
	}
	if strings.TrimSpace(settings.IPAddress) != "" {
		return strings.TrimSpace(settings.IPAddress)
	}
	if settings.Networks == nil {
		return ""
	}
	names := make([]string, 0, len(settings.Networks))
	for name := range settings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if network := settings.Networks[name]; network != nil && strings.TrimSpace(network.IPAddress) != "" {
			return strings.TrimSpace(network.IPAddress)
		}
	}
	return ""
}

func (r *DockerRuntime) waitHealthy(ctx context.Context, envdURL string) error {
	deadline := time.Now().Add(time.Duration(r.cfg.HealthTimeoutSeconds) * time.Second)
	healthURL := strings.TrimRight(envdURL, "/") + "/health"
	client := http.Client{Timeout: time.Second}

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("create envd health request: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("envd did not become healthy at %s within %ds", healthURL, r.cfg.HealthTimeoutSeconds)
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *DockerRuntime) waitReadyCommand(ctx context.Context, containerID string, readyCmd string) error {
	readyCmd = strings.TrimSpace(readyCmd)
	if readyCmd == "" {
		return nil
	}

	deadline := time.Now().Add(time.Duration(r.cfg.HealthTimeoutSeconds) * time.Second)
	var lastError string

	for {
		output, exitCode, err := r.runContainerReadyCommand(ctx, containerID, readyCmd)
		switch {
		case err == nil && exitCode == 0:
			return nil
		case err != nil:
			lastError = err.Error()
		default:
			lastError = readyCommandFailure(exitCode, output)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("template ready command did not succeed within %ds: %s", r.cfg.HealthTimeoutSeconds, lastError)
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *DockerRuntime) runContainerReadyCommand(ctx context.Context, containerID string, readyCmd string) (string, int, error) {
	exec, err := r.client.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          dockerReadyCommand(readyCmd),
	})
	if err != nil {
		return "", -1, fmt.Errorf("create ready command exec: %w", err)
	}

	attached, err := r.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", -1, fmt.Errorf("attach ready command exec: %w", err)
	}
	defer attached.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil {
		return strings.TrimSpace(stdout.String() + stderr.String()), -1, fmt.Errorf("read ready command output: %w", err)
	}

	inspect, err := r.client.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return strings.TrimSpace(stdout.String() + stderr.String()), -1, fmt.Errorf("inspect ready command exec: %w", err)
	}

	return strings.TrimSpace(strings.Join(nonEmptyStrings([]string{stdout.String(), stderr.String()}), "\n")), inspect.ExitCode, nil
}

func readyCommandFailure(exitCode int, output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return fmt.Sprintf("exit code %d", exitCode)
	}
	return fmt.Sprintf("exit code %d: %s", exitCode, output)
}

func (r *DockerRuntime) removeContainer(ctx context.Context, containerID string) error {
	err := r.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove docker container: %w", err)
	}
	return nil
}

func (r *DockerRuntime) containerLogs(ctx context.Context, containerID string) string {
	reader, err := r.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "80",
	})
	if err != nil {
		return ""
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

func dockerLogEntries(text string, level e2bapi.LogLevel, req SandboxLogsRequest) []SandboxRuntimeLogEntry {
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	entries := []SandboxRuntimeLogEntry{}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		timestamp, message := parseDockerLogLine(line)
		if !logEntryMatchesCursor(timestamp, req) {
			continue
		}
		if req.Search != nil && !strings.Contains(message, *req.Search) {
			continue
		}
		if req.Level != nil && logLevelRank(level) < logLevelRank(*req.Level) {
			continue
		}
		entries = append(entries, SandboxRuntimeLogEntry{
			Timestamp: timestamp,
			Level:     level,
			Message:   message,
			Fields: map[string]string{
				"source": "docker",
			},
		})
	}
	return entries
}

func dockerLogTail(req SandboxLogsRequest) int {
	if req.Limit <= 0 {
		return 0
	}
	if req.Start != nil && *req.Start > 0 {
		return 0
	}
	if req.Cursor != nil && *req.Cursor > 0 {
		return 0
	}
	tail := int(req.Limit)
	if req.Search != nil || req.Level != nil {
		tail *= 10
	}
	if tail > 5000 {
		return 5000
	}
	return tail
}

func parseDockerLogLine(line string) (time.Time, string) {
	separator := strings.IndexByte(line, ' ')
	if separator <= 0 {
		return time.Now().UTC(), line
	}

	timestamp, err := time.Parse(time.RFC3339Nano, line[:separator])
	if err != nil {
		return time.Now().UTC(), line
	}

	return timestamp.UTC(), line[separator+1:]
}

func logEntryMatchesCursor(timestamp time.Time, req SandboxLogsRequest) bool {
	timestampMS := timestamp.UnixMilli()
	if req.Start != nil && *req.Start > 0 && timestampMS < *req.Start {
		return false
	}
	if req.Cursor != nil && *req.Cursor > 0 && timestampMS < *req.Cursor {
		return false
	}
	return true
}

func limitDockerLogEntries(entries []SandboxRuntimeLogEntry, limit int32) []SandboxRuntimeLogEntry {
	if limit <= 0 || int(limit) >= len(entries) {
		return entries
	}
	return entries[:int(limit)]
}

func dockerCPUUsedPct(stats container.StatsResponse) float32 {
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage) - float64(stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage) - float64(stats.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || systemDelta <= 0 {
		return 0
	}

	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	return float32((cpuDelta / systemDelta) * onlineCPUs * 100)
}

func dockerMemoryCache(stats container.StatsResponse) uint64 {
	if stats.MemoryStats.Stats == nil {
		return 0
	}
	if value, ok := stats.MemoryStats.Stats["cache"]; ok {
		return value
	}
	if value, ok := stats.MemoryStats.Stats["total_inactive_file"]; ok {
		return value
	}
	return 0
}

func uint64ToInt64(value uint64) int64 {
	maxInt64 := uint64(1<<63 - 1)
	if value > maxInt64 {
		return int64(maxInt64)
	}
	return int64(value)
}

func logLevelRank(level e2bapi.LogLevel) int {
	switch level {
	case e2bapi.LogLevelDebug:
		return 1
	case e2bapi.LogLevelInfo:
		return 2
	case e2bapi.LogLevelWarn:
		return 3
	case e2bapi.LogLevelError:
		return 4
	default:
		return 0
	}
}

func envVars(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}

	return env
}

func parsePlatform(value string) *ocispec.Platform {
	if value == "" {
		return nil
	}

	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return nil
	}

	platform := &ocispec.Platform{
		OS:           parts[0],
		Architecture: parts[1],
	}

	if len(parts) > 2 {
		platform.Variant = parts[2]
	}

	return platform
}
