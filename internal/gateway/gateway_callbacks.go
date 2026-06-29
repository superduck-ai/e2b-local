package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"

	"github.com/google/uuid"
)

const (
	defaultSandboxTimeoutSeconds = 300
	DefaultSandboxTimeoutSeconds = defaultSandboxTimeoutSeconds
	maxSandboxListLimit          = 100
	MaxSandboxListLimit          = maxSandboxListLimit
	localTeamID                  = "00000000-0000-0000-0000-000000000001"
	localNodeID                  = "local"
)

type GatewayCallbacks struct {
	Health                func(ctx context.Context) error
	CreateSandbox         func(ctx context.Context, req e2bapi.NewSandbox) (SandboxRecord, error)
	ListSandboxes         func(ctx context.Context, req SandboxListRequest) (SandboxListResult, error)
	GetSandbox            func(ctx context.Context, sandboxID string) (SandboxRecord, error)
	KillSandbox           func(ctx context.Context, sandboxID string) error
	PauseSandbox          func(ctx context.Context, sandboxID string) (SandboxRecord, error)
	ResumeSandbox         func(ctx context.Context, sandboxID string, req e2bapi.ResumedSandbox) (SandboxRecord, error)
	ConnectSandbox        func(ctx context.Context, sandboxID string, req e2bapi.ConnectSandbox) (SandboxRecord, bool, error)
	SetSandboxTimeout     func(ctx context.Context, sandboxID string, req e2bapi.PostSandboxesSandboxIDTimeoutJSONBody) error
	RefreshSandbox        func(ctx context.Context, sandboxID string, req e2bapi.PostSandboxesSandboxIDRefreshesJSONBody) error
	GetSandboxMetrics     func(ctx context.Context, sandboxID string, req SandboxMetricsRequest) ([]e2bapi.SandboxMetric, error)
	ListSandboxMetrics    func(ctx context.Context, sandboxIDs []string) (e2bapi.SandboxesWithMetrics, error)
	CreateSandboxSnapshot func(ctx context.Context, sandboxID string, req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) (e2bapi.SnapshotInfo, error)
	ListSnapshots         func(ctx context.Context, req SnapshotListRequest) ([]e2bapi.SnapshotInfo, error)
	GetSandboxLogs        func(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error)
	GetSandboxLogsV2      func(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error)
	ListTemplates         func(ctx context.Context, params e2bapi.GetTemplatesParams) ([]GatewayTemplate, error)
	GetTemplate           func(ctx context.Context, templateID string, params e2bapi.GetTemplatesTemplateIDParams) (e2bapi.TemplateWithBuilds, error)
	GetTemplateAlias      func(ctx context.Context, alias string) (e2bapi.TemplateAliasResponse, error)
	CreateTemplate        func(ctx context.Context, req e2bapi.TemplateBuildRequest) (e2bapi.TemplateLegacy, error)
	CreateTemplateV2      func(ctx context.Context, req e2bapi.TemplateBuildRequestV2) (e2bapi.TemplateLegacy, error)
	CreateTemplateV3      func(ctx context.Context, req e2bapi.TemplateBuildRequestV3) (e2bapi.TemplateRequestResponseV3, error)
	DeleteTemplate        func(ctx context.Context, templateID string) error
	UpdateTemplate        func(ctx context.Context, templateID string, req e2bapi.TemplateUpdateRequest) error
	UpdateTemplateV2      func(ctx context.Context, templateID string, req e2bapi.TemplateUpdateRequest) (e2bapi.TemplateUpdateResponse, error)
	RebuildTemplate       func(ctx context.Context, templateID string, req e2bapi.TemplateBuildRequest) (e2bapi.TemplateLegacy, error)
	StartTemplateBuild    func(ctx context.Context, templateID string, buildID string) error
	StartTemplateBuildV2  func(ctx context.Context, templateID string, buildID string, req e2bapi.TemplateBuildStartV2) error
	GetTemplateBuildInfo  func(ctx context.Context, templateID string, buildID string, req TemplateBuildInfoRequest) (e2bapi.TemplateBuildInfo, error)
	GetTemplateBuildLogs  func(ctx context.Context, templateID string, buildID string, req TemplateBuildLogsRequest) (e2bapi.TemplateBuildLogsResponse, error)
	GetTemplateFileUpload func(ctx context.Context, templateID string, hash string) (e2bapi.TemplateBuildFileUpload, error)
	AssignTemplateTags    func(ctx context.Context, req e2bapi.AssignTemplateTagsRequest) (e2bapi.AssignedTemplateTags, error)
	DeleteTemplateTags    func(ctx context.Context, req e2bapi.DeleteTemplateTagsRequest) error
	ListTemplateTags      func(ctx context.Context, templateID string) ([]e2bapi.TemplateTag, error)
	ListNodes             func(ctx context.Context) ([]e2bapi.Node, error)
	GetNode               func(ctx context.Context, nodeID string, req NodeDetailRequest) (e2bapi.NodeDetail, error)
	SetNodeStatus         func(ctx context.Context, nodeID string, req e2bapi.NodeStatusChange) error
	ListTeams             func(ctx context.Context) ([]e2bapi.Team, error)
	GetTeamMetrics        func(ctx context.Context, teamID string, req TeamMetricsRequest) ([]e2bapi.TeamMetric, error)
	GetTeamMetricMax      func(ctx context.Context, teamID string, req TeamMetricMaxRequest) (e2bapi.MaxTeamMetric, error)
	KillTeamSandboxes     func(ctx context.Context, teamID string) (e2bapi.AdminSandboxKillResult, error)
	CancelTeamBuilds      func(ctx context.Context, teamID string) (e2bapi.AdminBuildCancelResult, error)
	ListVolumes           func(ctx context.Context) ([]e2bapi.Volume, error)
	CreateVolume          func(ctx context.Context, req e2bapi.NewVolume) (e2bapi.VolumeAndToken, error)
	GetVolume             func(ctx context.Context, volumeID string) (e2bapi.VolumeAndToken, error)
	DeleteVolume          func(ctx context.Context, volumeID string) error
}

func (callbacks GatewayCallbacks) WithDefaults(defaults GatewayCallbacks) GatewayCallbacks {
	if callbacks.Health == nil {
		callbacks.Health = defaults.Health
	}
	if callbacks.CreateSandbox == nil {
		callbacks.CreateSandbox = defaults.CreateSandbox
	}
	if callbacks.ListSandboxes == nil {
		callbacks.ListSandboxes = defaults.ListSandboxes
	}
	if callbacks.GetSandbox == nil {
		callbacks.GetSandbox = defaults.GetSandbox
	}
	if callbacks.KillSandbox == nil {
		callbacks.KillSandbox = defaults.KillSandbox
	}
	if callbacks.PauseSandbox == nil {
		callbacks.PauseSandbox = defaults.PauseSandbox
	}
	if callbacks.ResumeSandbox == nil {
		callbacks.ResumeSandbox = defaults.ResumeSandbox
	}
	if callbacks.ConnectSandbox == nil {
		callbacks.ConnectSandbox = defaults.ConnectSandbox
	}
	if callbacks.SetSandboxTimeout == nil {
		callbacks.SetSandboxTimeout = defaults.SetSandboxTimeout
	}
	if callbacks.RefreshSandbox == nil {
		callbacks.RefreshSandbox = defaults.RefreshSandbox
	}
	if callbacks.GetSandboxMetrics == nil {
		callbacks.GetSandboxMetrics = defaults.GetSandboxMetrics
	}
	if callbacks.ListSandboxMetrics == nil {
		callbacks.ListSandboxMetrics = defaults.ListSandboxMetrics
	}
	if callbacks.CreateSandboxSnapshot == nil {
		callbacks.CreateSandboxSnapshot = defaults.CreateSandboxSnapshot
	}
	if callbacks.ListSnapshots == nil {
		callbacks.ListSnapshots = defaults.ListSnapshots
	}
	if callbacks.GetSandboxLogs == nil {
		callbacks.GetSandboxLogs = defaults.GetSandboxLogs
	}
	if callbacks.GetSandboxLogsV2 == nil {
		callbacks.GetSandboxLogsV2 = defaults.GetSandboxLogsV2
	}
	if callbacks.ListTemplates == nil {
		callbacks.ListTemplates = defaults.ListTemplates
	}
	if callbacks.GetTemplate == nil {
		callbacks.GetTemplate = defaults.GetTemplate
	}
	if callbacks.GetTemplateAlias == nil {
		callbacks.GetTemplateAlias = defaults.GetTemplateAlias
	}
	if callbacks.CreateTemplate == nil {
		callbacks.CreateTemplate = defaults.CreateTemplate
	}
	if callbacks.CreateTemplateV2 == nil {
		callbacks.CreateTemplateV2 = defaults.CreateTemplateV2
	}
	if callbacks.CreateTemplateV3 == nil {
		callbacks.CreateTemplateV3 = defaults.CreateTemplateV3
	}
	if callbacks.DeleteTemplate == nil {
		callbacks.DeleteTemplate = defaults.DeleteTemplate
	}
	if callbacks.UpdateTemplate == nil {
		callbacks.UpdateTemplate = defaults.UpdateTemplate
	}
	if callbacks.UpdateTemplateV2 == nil {
		callbacks.UpdateTemplateV2 = defaults.UpdateTemplateV2
	}
	if callbacks.RebuildTemplate == nil {
		callbacks.RebuildTemplate = defaults.RebuildTemplate
	}
	if callbacks.StartTemplateBuild == nil {
		callbacks.StartTemplateBuild = defaults.StartTemplateBuild
	}
	if callbacks.StartTemplateBuildV2 == nil {
		callbacks.StartTemplateBuildV2 = defaults.StartTemplateBuildV2
	}
	if callbacks.GetTemplateBuildInfo == nil {
		callbacks.GetTemplateBuildInfo = defaults.GetTemplateBuildInfo
	}
	if callbacks.GetTemplateBuildLogs == nil {
		callbacks.GetTemplateBuildLogs = defaults.GetTemplateBuildLogs
	}
	if callbacks.GetTemplateFileUpload == nil {
		callbacks.GetTemplateFileUpload = defaults.GetTemplateFileUpload
	}
	if callbacks.AssignTemplateTags == nil {
		callbacks.AssignTemplateTags = defaults.AssignTemplateTags
	}
	if callbacks.DeleteTemplateTags == nil {
		callbacks.DeleteTemplateTags = defaults.DeleteTemplateTags
	}
	if callbacks.ListTemplateTags == nil {
		callbacks.ListTemplateTags = defaults.ListTemplateTags
	}
	if callbacks.ListNodes == nil {
		callbacks.ListNodes = defaults.ListNodes
	}
	if callbacks.GetNode == nil {
		callbacks.GetNode = defaults.GetNode
	}
	if callbacks.SetNodeStatus == nil {
		callbacks.SetNodeStatus = defaults.SetNodeStatus
	}
	if callbacks.ListTeams == nil {
		callbacks.ListTeams = defaults.ListTeams
	}
	if callbacks.GetTeamMetrics == nil {
		callbacks.GetTeamMetrics = defaults.GetTeamMetrics
	}
	if callbacks.GetTeamMetricMax == nil {
		callbacks.GetTeamMetricMax = defaults.GetTeamMetricMax
	}
	if callbacks.KillTeamSandboxes == nil {
		callbacks.KillTeamSandboxes = defaults.KillTeamSandboxes
	}
	if callbacks.CancelTeamBuilds == nil {
		callbacks.CancelTeamBuilds = defaults.CancelTeamBuilds
	}
	if callbacks.ListVolumes == nil {
		callbacks.ListVolumes = defaults.ListVolumes
	}
	if callbacks.CreateVolume == nil {
		callbacks.CreateVolume = defaults.CreateVolume
	}
	if callbacks.GetVolume == nil {
		callbacks.GetVolume = defaults.GetVolume
	}
	if callbacks.DeleteVolume == nil {
		callbacks.DeleteVolume = defaults.DeleteVolume
	}
	return callbacks
}

func DefaultGatewayCallbacks(app *App) GatewayCallbacks {
	return GatewayCallbacks{
		Health:                app.defaultHealth,
		CreateSandbox:         app.defaultCreateSandbox,
		ListSandboxes:         app.defaultListSandboxes,
		GetSandbox:            app.defaultGetSandbox,
		KillSandbox:           app.defaultKillSandbox,
		PauseSandbox:          app.defaultPauseSandbox,
		ResumeSandbox:         app.defaultResumeSandbox,
		ConnectSandbox:        app.defaultConnectSandbox,
		SetSandboxTimeout:     app.defaultSetSandboxTimeout,
		RefreshSandbox:        app.defaultRefreshSandbox,
		GetSandboxMetrics:     app.defaultGetSandboxMetrics,
		ListSandboxMetrics:    app.defaultListSandboxMetrics,
		CreateSandboxSnapshot: app.defaultCreateSandboxSnapshot,
		ListSnapshots:         app.defaultListSnapshots,
		GetSandboxLogs:        app.defaultGetSandboxLogs,
		GetSandboxLogsV2:      app.defaultGetSandboxLogs,
		ListTemplates:         app.defaultListTemplates,
		GetTemplate:           app.defaultGetTemplate,
		GetTemplateAlias:      app.defaultGetTemplateAlias,
		CreateTemplate:        app.defaultCreateTemplate,
		CreateTemplateV2:      app.defaultCreateTemplateV2,
		CreateTemplateV3:      app.defaultCreateTemplateV3,
		DeleteTemplate:        app.defaultDeleteTemplate,
		UpdateTemplate:        app.defaultUpdateTemplate,
		UpdateTemplateV2:      app.defaultUpdateTemplateV2,
		RebuildTemplate:       app.defaultRebuildTemplate,
		StartTemplateBuild:    app.defaultStartTemplateBuild,
		StartTemplateBuildV2:  app.defaultStartTemplateBuildV2,
		GetTemplateBuildInfo:  app.defaultGetTemplateBuildInfo,
		GetTemplateBuildLogs:  app.defaultGetTemplateBuildLogs,
		GetTemplateFileUpload: app.defaultGetTemplateFileUpload,
		AssignTemplateTags:    app.defaultAssignTemplateTags,
		DeleteTemplateTags:    app.defaultDeleteTemplateTags,
		ListTemplateTags:      app.defaultListTemplateTags,
		ListNodes:             app.defaultListNodes,
		GetNode:               app.defaultGetNode,
		SetNodeStatus:         app.defaultSetNodeStatus,
		ListTeams:             app.defaultListTeams,
		GetTeamMetrics:        app.defaultGetTeamMetrics,
		GetTeamMetricMax:      app.defaultGetTeamMetricMax,
		KillTeamSandboxes:     app.defaultKillTeamSandboxes,
		CancelTeamBuilds:      app.defaultCancelTeamBuilds,
		ListVolumes:           app.defaultListVolumes,
		CreateVolume:          app.defaultCreateVolume,
		GetVolume:             app.defaultGetVolume,
		DeleteVolume:          app.defaultDeleteVolume,
	}
}

type GatewayTemplate struct {
	e2bapi.Template
	ImageRef string `json:"imageRef,omitempty"`
}

type SandboxListRequest struct {
	Metadata  map[string]string
	States    []e2bapi.SandboxState
	NextToken string
	Limit     int
	V2        bool
}

type SandboxListResult struct {
	Items        []SandboxRecord
	NextToken    string
	TotalRunning int
}

type SandboxLogsRequest struct {
	Cursor    *int64
	Start     *int64
	Limit     int32
	Direction *e2bapi.LogsDirection
	Level     *e2bapi.LogLevel
	Search    *string
}

type SandboxRuntimeLogEntry struct {
	Timestamp time.Time
	Level     e2bapi.LogLevel
	Message   string
	Fields    map[string]string
}

type SandboxMetricsRequest struct {
	Start *int64
	End   *int64
}

type SandboxRuntimeMetrics interface {
	GetSandboxMetrics(ctx context.Context, record SandboxRecord, req SandboxMetricsRequest) ([]e2bapi.SandboxMetric, error)
}

type TeamMetricsRequest struct {
	Start *int64
	End   *int64
}

type TeamMetricMaxRequest struct {
	Start  *int64
	End    *int64
	Metric e2bapi.GetTeamsTeamIDMetricsMaxParamsMetric
}

type TemplateBuildInfoRequest struct {
	LogsOffset *int32
	Limit      *int32
	Level      *e2bapi.LogLevel
}

type TemplateBuildLogsRequest struct {
	Cursor    *int64
	Limit     *int32
	Direction *e2bapi.LogsDirection
	Level     *e2bapi.LogLevel
	Source    *e2bapi.LogsSource
}

type NodeDetailRequest struct {
	ClusterID *uuid.UUID
}

type SnapshotListRequest struct {
	SandboxID string
	NextToken string
	Limit     int
}

type SandboxRuntimeSnapshotter interface {
	CreateSandboxSnapshot(ctx context.Context, record SandboxRecord, req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) (e2bapi.SnapshotInfo, error)
	ListSnapshots(ctx context.Context, req SnapshotListRequest) ([]e2bapi.SnapshotInfo, error)
}

type SandboxRuntimeLogger interface {
	GetSandboxLogs(ctx context.Context, info SandboxRuntimeInfo, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error)
}

type SandboxRuntimeTemplateBuilder interface {
	BuildTemplate(ctx context.Context, template GatewayTemplate, req e2bapi.TemplateBuildRequest) (GatewayTemplate, []e2bapi.BuildLogEntry, error)
}

type SandboxRuntimeTemplateBuildStarter interface {
	StartTemplateBuildV2(ctx context.Context, template GatewayTemplate, buildID string, req e2bapi.TemplateBuildStartV2, files []TemplateBuildFile) (GatewayTemplate, []e2bapi.BuildLogEntry, error)
}

type GatewayError struct {
	Status  int
	Message string
}

func (e GatewayError) Error() string {
	return e.Message
}

func NewGatewayError(status int, format string, args ...any) GatewayError {
	return gatewayError(status, format, args...)
}

func gatewayError(status int, format string, args ...any) GatewayError {
	return GatewayError{
		Status:  status,
		Message: fmt.Sprintf(format, args...),
	}
}

func GatewayErrorStatus(err error, fallback int) int {
	return gatewayErrorStatus(err, fallback)
}

func gatewayErrorStatus(err error, fallback int) int {
	if gatewayErr, ok := asGatewayError(err); ok {
		return gatewayErr.Status
	}
	return fallback
}

func asNotFoundGatewayError(err error) bool {
	if err == nil {
		return false
	}
	gatewayErr, ok := asGatewayError(err)
	return ok && gatewayErr.Status == http.StatusNotFound
}

func asGatewayError(err error) (GatewayError, bool) {
	var gatewayErr GatewayError
	if errors.As(err, &gatewayErr) {
		return gatewayErr, true
	}
	return GatewayError{}, false
}

func (a *App) defaultHealth(ctx context.Context) error {
	return nil
}

func (a *App) defaultCreateSandbox(ctx context.Context, req e2bapi.NewSandbox) (SandboxRecord, error) {
	templateID := strings.TrimSpace(req.TemplateID)
	if templateID == "" {
		return SandboxRecord{}, gatewayError(http.StatusBadRequest, "templateID is required")
	}

	if err := a.validateTemplateID(templateID); err != nil {
		return SandboxRecord{}, gatewayError(http.StatusBadRequest, "%s", err.Error())
	}

	sandboxID, err := newSandboxID()
	if err != nil {
		return SandboxRecord{}, err
	}

	metadata := sandboxMetadata(req.Metadata)
	volumeMounts := sandboxVolumeMounts(req.VolumeMounts)
	envVars := sandboxEnvVars(req.EnvVars)
	timeoutSeconds := requestTimeout(req.Timeout)
	now := time.Now().UTC()
	endAt := now.Add(time.Duration(timeoutSeconds) * time.Second)

	runtimeInfo, err := a.runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:           sandboxID,
		TemplateID:          templateID,
		Metadata:            metadata,
		EnvVars:             envVars,
		VolumeMounts:        volumeMounts,
		CreatedAt:           now,
		EndAt:               endAt,
		AllowInternetAccess: req.AllowInternetAccess,
	})
	if err != nil {
		if errors.Is(err, ErrRuntimeCapacity) {
			return SandboxRecord{}, gatewayError(http.StatusServiceUnavailable, "%s", err.Error())
		}
		return SandboxRecord{}, err
	}

	record, err := a.store.Create(SandboxRecord{
		ID:                  sandboxID,
		TemplateID:          templateID,
		Metadata:            metadata,
		EnvdURL:             runtimeInfo.EnvdURL,
		RuntimeInfo:         runtimeInfo,
		CreatedAt:           now,
		EndAt:               endAt,
		State:               string(e2bapi.Running),
		AllowInternetAccess: req.AllowInternetAccess,
	})
	if err != nil {
		if cleanupErr := a.runtime.DeleteSandbox(context.Background(), runtimeInfo); cleanupErr != nil {
			a.logger.Printf("sandbox create cleanup failed sandbox_id=%s error=%v", sandboxID, cleanupErr)
		}
		return SandboxRecord{}, err
	}

	a.logger.Printf("sandbox create sandbox_id=%s template_id=%s envd_url=%s container_id=%s",
		record.ID,
		record.TemplateID,
		record.EnvdURL,
		record.RuntimeInfo.ContainerID,
	)

	return record, nil
}

func (a *App) defaultListSandboxes(ctx context.Context, req SandboxListRequest) (SandboxListResult, error) {
	records, err := a.reconcileSandboxRecords(ctx, a.store.List())
	if err != nil {
		return SandboxListResult{}, err
	}
	stateFilter := sandboxStateFilter(req.States)
	filtered := make([]SandboxRecord, 0, len(records))
	totalRunning := 0

	for _, record := range records {
		if !metadataMatches(record.Metadata, req.Metadata) {
			continue
		}
		if record.State == string(e2bapi.Running) {
			totalRunning++
		}
		if len(stateFilter) > 0 && !stateFilter[record.State] {
			continue
		}
		filtered = append(filtered, record)
	}

	start := 0
	if req.NextToken != "" {
		start = len(filtered)
		for i, record := range filtered {
			if record.ID == req.NextToken {
				start = i + 1
				break
			}
		}
	}

	if start > len(filtered) {
		start = len(filtered)
	}

	limit := req.Limit
	if limit <= 0 || limit > maxSandboxListLimit {
		limit = maxSandboxListLimit
	}

	end := start + limit
	result := SandboxListResult{TotalRunning: totalRunning}
	if end < len(filtered) {
		result.Items = filtered[start:end]
		result.NextToken = filtered[end-1].ID
	} else {
		result.Items = filtered[start:]
	}
	return result, nil
}

func (a *App) defaultGetSandbox(ctx context.Context, sandboxID string) (SandboxRecord, error) {
	return a.activeSandboxRecord(ctx, sandboxID)
}

func (a *App) activeSandboxRecord(ctx context.Context, sandboxID string) (SandboxRecord, error) {
	record, ok := a.store.Get(sandboxID)
	if !ok {
		return SandboxRecord{}, gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}
	record, exists, err := a.reconcileSandboxRecord(ctx, record)
	if err != nil {
		return SandboxRecord{}, err
	}
	if !exists {
		return SandboxRecord{}, gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}
	return record, nil
}

func (a *App) reconcileSandboxRecords(ctx context.Context, records []SandboxRecord) ([]SandboxRecord, error) {
	reconciled := make([]SandboxRecord, 0, len(records))
	for _, record := range records {
		record, exists, err := a.reconcileSandboxRecord(ctx, record)
		if err != nil {
			return nil, err
		}
		if exists {
			reconciled = append(reconciled, record)
		}
	}
	return reconciled, nil
}

func (a *App) reconcileSandboxRecord(ctx context.Context, record SandboxRecord) (SandboxRecord, bool, error) {
	if sandboxRecordExpired(record, time.Now().UTC()) {
		if a.runtime != nil {
			if err := a.runtime.DeleteSandbox(ctx, record.RuntimeInfo); err != nil {
				return SandboxRecord{}, false, err
			}
		}
		deleted, err := a.store.Delete(record.ID)
		if err != nil {
			return SandboxRecord{}, false, err
		}
		if deleted {
			a.logger.Printf("sandbox reconcile removed expired sandbox_id=%s container_id=%s", record.ID, record.RuntimeInfo.ContainerID)
		}
		return SandboxRecord{}, false, nil
	}

	inspector, ok := a.runtime.(SandboxRuntimeInspector)
	if !ok {
		return record, true, nil
	}
	return a.reconcileSandboxRecordWithInspector(ctx, inspector, record)
}

func (a *App) reconcileSandboxRecordWithInspector(ctx context.Context, inspector SandboxRuntimeInspector, record SandboxRecord) (SandboxRecord, bool, error) {
	inspection, err := inspector.InspectSandbox(ctx, record.RuntimeInfo)
	if err != nil {
		return SandboxRecord{}, false, err
	}
	if !inspection.Exists {
		if a.runtime != nil {
			if err := a.runtime.DeleteSandbox(ctx, record.RuntimeInfo); err != nil {
				a.logger.Printf("sandbox reconcile cleanup failed sandbox_id=%s container_id=%s error=%v", record.ID, record.RuntimeInfo.ContainerID, err)
			}
		}
		deleted, err := a.store.Delete(record.ID)
		if err != nil {
			return SandboxRecord{}, false, err
		}
		if deleted {
			a.logger.Printf("sandbox reconcile removed missing sandbox_id=%s container_id=%s", record.ID, record.RuntimeInfo.ContainerID)
		}
		return SandboxRecord{}, false, nil
	}

	state := strings.TrimSpace(inspection.State)
	if state == "" {
		state = record.State
	}
	runtimeInfo := mergeSandboxRuntimeInfo(record.RuntimeInfo, inspection.Info)
	updated, ok, err := a.store.SetStateRuntimeInfoAndEndAt(record.ID, state, runtimeInfo, time.Time{})
	if err != nil {
		return SandboxRecord{}, false, err
	}
	if !ok {
		return SandboxRecord{}, false, nil
	}
	return updated, true, nil
}

func sandboxRecordExpired(record SandboxRecord, now time.Time) bool {
	endAt := recordEndAt(record)
	return !endAt.IsZero() && !now.Before(endAt)
}

func mergeSandboxRuntimeInfo(existing SandboxRuntimeInfo, update SandboxRuntimeInfo) SandboxRuntimeInfo {
	if update.SandboxID == "" {
		update.SandboxID = existing.SandboxID
	}
	if update.EnvdURL == "" {
		update.EnvdURL = existing.EnvdURL
	}
	if update.ContainerID == "" {
		update.ContainerID = existing.ContainerID
	}
	if update.ContainerName == "" {
		update.ContainerName = existing.ContainerName
	}
	if update.ContainerIP == "" {
		update.ContainerIP = existing.ContainerIP
	}
	if update.HostPort == "" {
		update.HostPort = existing.HostPort
	}
	if update.MachineID == "" {
		update.MachineID = existing.MachineID
	}
	if len(update.VolumeMounts) == 0 {
		update.VolumeMounts = existing.VolumeMounts
	}
	if len(update.PublishedPorts) == 0 {
		update.PublishedPorts = existing.PublishedPorts
	}
	return update
}

func (a *App) defaultKillSandbox(ctx context.Context, sandboxID string) error {
	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return err
	}

	if err := a.runtime.DeleteSandbox(ctx, record.RuntimeInfo); err != nil {
		return err
	}

	deleted, err := a.store.Delete(sandboxID)
	if err != nil {
		return err
	}
	if !deleted {
		return gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}

	a.logger.Printf("sandbox delete sandbox_id=%s action=delete_mapping container_id=%s", sandboxID, record.RuntimeInfo.ContainerID)
	return nil
}

func (a *App) defaultPauseSandbox(ctx context.Context, sandboxID string) (SandboxRecord, error) {
	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return SandboxRecord{}, err
	}
	if record.State == string(e2bapi.Paused) {
		return SandboxRecord{}, gatewayError(http.StatusConflict, "sandbox %s is already paused", sandboxID)
	}

	if err := a.runtime.PauseSandbox(ctx, record.RuntimeInfo); err != nil {
		return SandboxRecord{}, err
	}

	record, ok, err := a.store.SetState(sandboxID, string(e2bapi.Paused))
	if err != nil {
		return SandboxRecord{}, err
	}
	if !ok {
		return SandboxRecord{}, gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}

	a.logger.Printf("sandbox pause sandbox_id=%s action=mark_paused", record.ID)
	return record, nil
}

func (a *App) defaultResumeSandbox(ctx context.Context, sandboxID string, req e2bapi.ResumedSandbox) (SandboxRecord, error) {
	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return SandboxRecord{}, err
	}
	if record.State == string(e2bapi.Running) {
		return SandboxRecord{}, gatewayError(http.StatusConflict, "sandbox %s is already running", sandboxID)
	}

	runtimeInfo, err := a.runtime.ResumeSandbox(ctx, record.RuntimeInfo)
	if err != nil {
		return SandboxRecord{}, err
	}

	endAt := record.EndAt
	if req.Timeout != nil && *req.Timeout > 0 {
		endAt = time.Now().UTC().Add(time.Duration(*req.Timeout) * time.Second)
	}
	record, ok, err := a.store.SetStateRuntimeInfoAndEndAt(sandboxID, string(e2bapi.Running), runtimeInfo, endAt)
	if err != nil {
		return SandboxRecord{}, err
	}
	if !ok {
		return SandboxRecord{}, gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}
	return record, nil
}

func (a *App) defaultConnectSandbox(ctx context.Context, sandboxID string, req e2bapi.ConnectSandbox) (SandboxRecord, bool, error) {
	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return SandboxRecord{}, false, err
	}

	resumed := false
	if record.State == string(e2bapi.Paused) {
		runtimeInfo, err := a.runtime.ResumeSandbox(ctx, record.RuntimeInfo)
		if err != nil {
			return SandboxRecord{}, false, err
		}
		endAt := record.EndAt
		if req.Timeout > 0 {
			endAt = time.Now().UTC().Add(time.Duration(req.Timeout) * time.Second)
		}
		updated, ok, err := a.store.SetStateRuntimeInfoAndEndAt(sandboxID, string(e2bapi.Running), runtimeInfo, endAt)
		if err != nil {
			return SandboxRecord{}, false, err
		}
		if !ok {
			return SandboxRecord{}, false, gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
		}
		record = updated
		resumed = true
	}

	a.logger.Printf("sandbox connect sandbox_id=%s envd_url=%s resumed=%t", record.ID, record.EnvdURL, resumed)
	return record, resumed, nil
}

func (a *App) defaultSetSandboxTimeout(ctx context.Context, sandboxID string, req e2bapi.PostSandboxesSandboxIDTimeoutJSONBody) error {
	if req.Timeout < 0 {
		return gatewayError(http.StatusBadRequest, "timeout must be greater than or equal to 0")
	}

	if _, err := a.activeSandboxRecord(ctx, sandboxID); err != nil {
		return err
	}

	endAt := time.Now().UTC().Add(time.Duration(req.Timeout) * time.Second)
	if _, ok, err := a.store.SetEndAt(sandboxID, endAt); err != nil {
		return err
	} else if !ok {
		return gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}

	a.logger.Printf("sandbox timeout sandbox_id=%s timeout_seconds=%d", sandboxID, req.Timeout)
	return nil
}

func (a *App) defaultRefreshSandbox(ctx context.Context, sandboxID string, req e2bapi.PostSandboxesSandboxIDRefreshesJSONBody) error {
	duration := defaultSandboxTimeoutSeconds
	if req.Duration != nil {
		duration = *req.Duration
	}
	if duration < 0 {
		return gatewayError(http.StatusBadRequest, "duration must be greater than or equal to 0")
	}
	if duration > 3600 {
		return gatewayError(http.StatusBadRequest, "duration must be less than or equal to 3600")
	}

	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return err
	}

	endAt := time.Now().UTC().Add(time.Duration(duration) * time.Second)
	if record.EndAt.After(endAt) {
		endAt = record.EndAt
	}
	if _, ok, err := a.store.SetEndAt(sandboxID, endAt); err != nil {
		return err
	} else if !ok {
		return gatewayError(http.StatusNotFound, "sandbox %s not found", sandboxID)
	}

	a.logger.Printf("sandbox refresh sandbox_id=%s duration_seconds=%d", sandboxID, duration)
	return nil
}

func (a *App) defaultGetSandboxMetrics(ctx context.Context, sandboxID string, req SandboxMetricsRequest) ([]e2bapi.SandboxMetric, error) {
	if err := validateMetricsRequest(req); err != nil {
		return nil, err
	}

	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	if metricsRuntime, ok := a.runtime.(SandboxRuntimeMetrics); ok {
		return metricsRuntime.GetSandboxMetrics(ctx, record, req)
	}

	metric := sandboxMetricFromRecord(record, time.Now().UTC())
	if !metricMatchesRange(metric, req) {
		return []e2bapi.SandboxMetric{}, nil
	}
	return []e2bapi.SandboxMetric{metric}, nil
}

func (a *App) defaultListSandboxMetrics(ctx context.Context, sandboxIDs []string) (e2bapi.SandboxesWithMetrics, error) {
	if len(sandboxIDs) == 0 {
		return e2bapi.SandboxesWithMetrics{}, gatewayError(http.StatusBadRequest, "sandbox_ids is required")
	}
	if len(sandboxIDs) > 100 {
		return e2bapi.SandboxesWithMetrics{}, gatewayError(http.StatusBadRequest, "sandbox_ids must contain at most 100 items")
	}

	result := e2bapi.SandboxesWithMetrics{
		Sandboxes: map[string]e2bapi.SandboxMetric{},
	}
	seen := map[string]struct{}{}
	for _, sandboxID := range sandboxIDs {
		sandboxID = strings.TrimSpace(sandboxID)
		if sandboxID == "" {
			return e2bapi.SandboxesWithMetrics{}, gatewayError(http.StatusBadRequest, "sandbox_ids must not contain empty values")
		}
		if _, ok := seen[sandboxID]; ok {
			continue
		}
		seen[sandboxID] = struct{}{}

		record, err := a.activeSandboxRecord(ctx, sandboxID)
		if err != nil {
			if asNotFoundGatewayError(err) {
				continue
			}
			return e2bapi.SandboxesWithMetrics{}, err
		}
		if apiSandboxState(record.State) != e2bapi.Running {
			continue
		}

		metrics, err := a.defaultGetSandboxMetrics(ctx, sandboxID, SandboxMetricsRequest{})
		if err != nil {
			return e2bapi.SandboxesWithMetrics{}, err
		}
		if len(metrics) > 0 {
			result.Sandboxes[sandboxID] = metrics[len(metrics)-1]
		}
	}
	return result, nil
}

func (a *App) defaultCreateSandboxSnapshot(ctx context.Context, sandboxID string, req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody) (e2bapi.SnapshotInfo, error) {
	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return e2bapi.SnapshotInfo{}, err
	}

	snapshotter, ok := a.runtime.(SandboxRuntimeSnapshotter)
	if !ok {
		return e2bapi.SnapshotInfo{}, gatewayError(http.StatusNotImplemented, "snapshots are not supported by this runtime")
	}

	snapshot, err := snapshotter.CreateSandboxSnapshot(ctx, record, req)
	if err != nil {
		return e2bapi.SnapshotInfo{}, err
	}

	a.logger.Printf("sandbox snapshot create sandbox_id=%s snapshot_id=%s", sandboxID, snapshot.SnapshotID)
	return snapshot, nil
}

func (a *App) defaultListSnapshots(ctx context.Context, req SnapshotListRequest) ([]e2bapi.SnapshotInfo, error) {
	snapshotter, ok := a.runtime.(SandboxRuntimeSnapshotter)
	if !ok {
		return nil, gatewayError(http.StatusNotImplemented, "snapshots are not supported by this runtime")
	}
	return snapshotter.ListSnapshots(ctx, req)
}

func (a *App) defaultGetSandboxLogs(ctx context.Context, sandboxID string, req SandboxLogsRequest) ([]SandboxRuntimeLogEntry, error) {
	record, err := a.activeSandboxRecord(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	logRuntime, ok := a.runtime.(SandboxRuntimeLogger)
	if !ok {
		return []SandboxRuntimeLogEntry{}, nil
	}
	return logRuntime.GetSandboxLogs(ctx, record.RuntimeInfo, req)
}

func (a *App) defaultListTemplates(ctx context.Context, params e2bapi.GetTemplatesParams) ([]GatewayTemplate, error) {
	templates, err := a.runtime.ListTemplates(ctx)
	if err != nil {
		return nil, err
	}

	deleted := a.management.DeletedTemplateIDs()
	result := make([]GatewayTemplate, 0, len(templates))
	indexByID := map[string]int{}
	for _, template := range templates {
		gatewayTemplate := a.gatewayTemplate(template)
		if deleted[gatewayTemplate.TemplateID] {
			continue
		}
		indexByID[gatewayTemplate.TemplateID] = len(result)
		result = append(result, gatewayTemplate)
	}
	for _, template := range a.management.ListManagedTemplates() {
		if deleted[template.TemplateID] {
			continue
		}
		if index, ok := indexByID[template.TemplateID]; ok {
			result[index] = template
			continue
		}
		indexByID[template.TemplateID] = len(result)
		result = append(result, template)
	}
	return result, nil
}

func (a *App) defaultGetTemplate(ctx context.Context, templateID string, params e2bapi.GetTemplatesTemplateIDParams) (e2bapi.TemplateWithBuilds, error) {
	limit, err := paginationLimit(params.Limit)
	if err != nil {
		return e2bapi.TemplateWithBuilds{}, err
	}

	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return e2bapi.TemplateWithBuilds{}, err
	}

	result := templateWithBuilds(template)
	if managedBuilds := a.management.TemplateBuilds(template.TemplateID); len(managedBuilds) > 0 {
		result.Builds = templateBuildRecords(managedBuilds)
	}
	if params.NextToken != nil && strings.TrimSpace(string(*params.NextToken)) != "" {
		result.Builds = []e2bapi.TemplateBuild{}
		return result, nil
	}
	if limit < len(result.Builds) {
		result.Builds = result.Builds[:limit]
	}
	return result, nil
}

func (a *App) defaultGetTemplateAlias(ctx context.Context, alias string) (e2bapi.TemplateAliasResponse, error) {
	template, err := a.findTemplate(ctx, alias)
	if err != nil {
		return e2bapi.TemplateAliasResponse{}, err
	}

	return e2bapi.TemplateAliasResponse{
		Public:     template.Public,
		TemplateID: template.TemplateID,
	}, nil
}

func (a *App) defaultCreateTemplate(ctx context.Context, req e2bapi.TemplateBuildRequest) (e2bapi.TemplateLegacy, error) {
	if strings.TrimSpace(req.Dockerfile) == "" {
		return e2bapi.TemplateLegacy{}, gatewayError(http.StatusBadRequest, "dockerfile is required")
	}

	name := stringPtrValue(req.Alias)
	if strings.TrimSpace(name) == "" {
		name = "template-" + uuid.NewString()[:8]
	}

	template, tags := a.newManagedTemplate(name, req.Alias, nil, req.CpuCount, req.MemoryMB)
	logs := localBuildLogs("template build accepted by e2b-local")
	if builder, ok := a.runtime.(SandboxRuntimeTemplateBuilder); ok {
		var err error
		template, logs, err = builder.BuildTemplate(ctx, template, req)
		if err != nil {
			return e2bapi.TemplateLegacy{}, gatewayError(gatewayErrorStatus(err, http.StatusBadGateway), "%s", err.Error())
		}
	}
	if _, err := a.management.UpsertTemplate(template, tags, logs); err != nil {
		return e2bapi.TemplateLegacy{}, err
	}
	return templateLegacy(template), nil
}

func (a *App) defaultCreateTemplateV2(ctx context.Context, req e2bapi.TemplateBuildRequestV2) (e2bapi.TemplateLegacy, error) {
	if strings.TrimSpace(req.Alias) == "" {
		return e2bapi.TemplateLegacy{}, gatewayError(http.StatusBadRequest, "alias is required")
	}

	alias := req.Alias
	template, tags := a.newManagedTemplate(req.Alias, &alias, nil, req.CpuCount, req.MemoryMB)
	if _, err := a.management.UpsertTemplate(template, tags, localBuildLogs("v2 template build accepted by e2b-local")); err != nil {
		return e2bapi.TemplateLegacy{}, err
	}
	return templateLegacy(template), nil
}

func (a *App) defaultCreateTemplateV3(ctx context.Context, req e2bapi.TemplateBuildRequestV3) (e2bapi.TemplateRequestResponseV3, error) {
	name := stringPtrValue(req.Name)
	if strings.TrimSpace(name) == "" {
		name = stringPtrValue(req.Alias)
	}
	if strings.TrimSpace(name) == "" {
		name = "template-" + uuid.NewString()[:8]
	}

	template, tags := a.newManagedTemplate(name, req.Alias, req.Tags, req.CpuCount, req.MemoryMB)
	if _, err := a.management.UpsertTemplate(template, tags, localBuildLogs("v3 template build accepted by e2b-local")); err != nil {
		return e2bapi.TemplateRequestResponseV3{}, err
	}
	return templateRequestResponseV3(template, tags), nil
}

func (a *App) defaultDeleteTemplate(ctx context.Context, templateID string) error {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return err
	}

	return a.management.DeleteTemplate(template.TemplateID)
}

func (a *App) defaultUpdateTemplate(ctx context.Context, templateID string, req e2bapi.TemplateUpdateRequest) error {
	_, err := a.defaultUpdateTemplateRecord(ctx, templateID, req)
	return err
}

func (a *App) defaultUpdateTemplateV2(ctx context.Context, templateID string, req e2bapi.TemplateUpdateRequest) (e2bapi.TemplateUpdateResponse, error) {
	template, err := a.defaultUpdateTemplateRecord(ctx, templateID, req)
	if err != nil {
		return e2bapi.TemplateUpdateResponse{}, err
	}
	return e2bapi.TemplateUpdateResponse{
		Names: append([]string(nil), template.Names...),
	}, nil
}

func (a *App) defaultUpdateTemplateRecord(ctx context.Context, templateID string, req e2bapi.TemplateUpdateRequest) (GatewayTemplate, error) {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return GatewayTemplate{}, err
	}
	if req.Public != nil {
		template.Public = *req.Public
	}
	template.UpdatedAt = time.Now().UTC()
	return a.management.UpsertTemplate(template, nil, nil)
}

func (a *App) defaultRebuildTemplate(ctx context.Context, templateID string, req e2bapi.TemplateBuildRequest) (e2bapi.TemplateLegacy, error) {
	if strings.TrimSpace(req.Dockerfile) == "" {
		return e2bapi.TemplateLegacy{}, gatewayError(http.StatusBadRequest, "dockerfile is required")
	}

	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return e2bapi.TemplateLegacy{}, err
	}

	now := time.Now().UTC()
	template.BuildCount++
	template.BuildID = uuid.NewString()
	template.BuildStatus = e2bapi.TemplateBuildStatusReady
	template.UpdatedAt = now
	if req.CpuCount != nil {
		template.CpuCount = *req.CpuCount
	}
	if req.MemoryMB != nil {
		template.MemoryMB = *req.MemoryMB
	}
	if req.Alias != nil {
		template.Aliases = appendUniqueStrings(template.Aliases, *req.Alias)
		template.Names = appendUniqueStrings(template.Names, *req.Alias)
	}

	logs := localBuildLogs("template rebuild accepted by e2b-local")
	if builder, ok := a.runtime.(SandboxRuntimeTemplateBuilder); ok {
		template, logs, err = builder.BuildTemplate(ctx, template, req)
		if err != nil {
			return e2bapi.TemplateLegacy{}, gatewayError(gatewayErrorStatus(err, http.StatusBadGateway), "%s", err.Error())
		}
	}
	if _, err := a.management.UpsertTemplate(template, nil, logs); err != nil {
		return e2bapi.TemplateLegacy{}, err
	}
	return templateLegacy(template), nil
}

func (a *App) defaultStartTemplateBuild(ctx context.Context, templateID string, buildID string) error {
	return a.defaultMarkTemplateBuildReady(ctx, templateID, buildID, "template build started by e2b-local")
}

func (a *App) defaultStartTemplateBuildV2(ctx context.Context, templateID string, buildID string, req e2bapi.TemplateBuildStartV2) error {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return err
	}

	buildID = strings.TrimSpace(buildID)
	if buildID == "" {
		return gatewayError(http.StatusBadRequest, "buildID is required")
	}

	template.BuildID = buildID
	template.BuildStatus = e2bapi.TemplateBuildStatusReady
	template.UpdatedAt = time.Now().UTC()
	startLogs := localBuildLogs("v2 template build started by e2b-local")

	if starter, ok := a.runtime.(SandboxRuntimeTemplateBuildStarter); ok {
		files, err := a.templateBuildFiles(req, template.TemplateID)
		if err != nil {
			return err
		}
		task, err := a.builds.reserve(template.TemplateID, buildID)
		if err != nil {
			return templateBuildStartError(err)
		}
		template.BuildStatus = e2bapi.TemplateBuildStatusBuilding
		if _, err := a.management.UpsertTemplate(template, nil, startLogs); err != nil {
			a.builds.release(task)
			return err
		}
		a.startTemplateBuildV2(task, starter, template, buildID, req, files, startLogs)
		return nil
	}

	_, err = a.management.UpsertTemplate(template, nil, startLogs)
	return err
}

func (a *App) startTemplateBuildV2(task *templateBuildTask, starter SandboxRuntimeTemplateBuildStarter, template GatewayTemplate, buildID string, req e2bapi.TemplateBuildStartV2, files []TemplateBuildFile, startLogs []e2bapi.BuildLogEntry) {
	task.goRun(func(ctx context.Context) {
		builtTemplate, logs, err := starter.StartTemplateBuildV2(ctx, template, buildID, req, files)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			failed := template
			failed.BuildID = buildID
			failed.BuildStatus = e2bapi.TemplateBuildStatusError
			failed.UpdatedAt = time.Now().UTC()
			message := err.Error()
			if errors.Is(err, context.Canceled) {
				message = "v2 template build cancelled by e2b-local"
			}
			if len(logs) == 0 {
				logs = localBuildErrorLogs(message)
			}
			if !a.builds.isCurrent(task) {
				return
			}
			if _, saveErr := a.management.UpsertTemplate(failed, nil, appendBuildLogs(startLogs, logs)); saveErr != nil {
				a.logger.Printf("v2 template build failure state save failed template_id=%s build_id=%s: %v", template.TemplateID, buildID, saveErr)
			}
			a.logger.Printf("v2 template build failed template_id=%s build_id=%s: %v", template.TemplateID, buildID, err)
			return
		}
		if !a.builds.isCurrent(task) {
			return
		}

		if strings.TrimSpace(builtTemplate.TemplateID) == "" {
			builtTemplate.TemplateID = template.TemplateID
		}
		if strings.TrimSpace(builtTemplate.BuildID) == "" {
			builtTemplate.BuildID = buildID
		}
		if strings.TrimSpace(string(builtTemplate.BuildStatus)) == "" || builtTemplate.BuildStatus == e2bapi.TemplateBuildStatusBuilding {
			builtTemplate.BuildStatus = e2bapi.TemplateBuildStatusReady
		}
		builtTemplate.UpdatedAt = time.Now().UTC()
		if _, saveErr := a.management.UpsertTemplate(builtTemplate, nil, appendBuildLogs(startLogs, logs)); saveErr != nil {
			a.logger.Printf("v2 template build state save failed template_id=%s build_id=%s: %v", template.TemplateID, buildID, saveErr)
		}
	})
}

func templateBuildStartError(err error) error {
	switch {
	case errors.Is(err, errTemplateBuildCapacityExhausted):
		return gatewayError(http.StatusTooManyRequests, "template build capacity exhausted")
	case errors.Is(err, errTemplateBuildAlreadyRunning):
		return gatewayError(http.StatusConflict, "template build is already running")
	case errors.Is(err, errTemplateBuildManagerStopped):
		return gatewayError(http.StatusServiceUnavailable, "template build manager is stopped")
	default:
		return err
	}
}

func (a *App) templateBuildFiles(req e2bapi.TemplateBuildStartV2, templateID string) ([]TemplateBuildFile, error) {
	files := []TemplateBuildFile{}
	seen := map[string]bool{}
	for index, step := range templateBuildSteps(req.Steps) {
		if !strings.EqualFold(strings.TrimSpace(step.Type), "COPY") {
			continue
		}
		hash := stringPtrValue(step.FilesHash)
		if hash == "" {
			return nil, gatewayError(http.StatusBadRequest, "template step %d: COPY requires filesHash", index)
		}
		if seen[hash] {
			continue
		}
		file, ok := a.management.TemplateFile(templateID, hash)
		if !ok {
			return nil, gatewayError(http.StatusBadRequest, "template step %d: uploaded file cache %s not found", index, hash)
		}
		seen[hash] = true
		files = append(files, file)
	}
	return files, nil
}

func (a *App) defaultMarkTemplateBuildReady(ctx context.Context, templateID string, buildID string, message string) error {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return err
	}
	buildID = strings.TrimSpace(buildID)
	if buildID == "" {
		return gatewayError(http.StatusBadRequest, "buildID is required")
	}

	template.BuildID = buildID
	template.BuildStatus = e2bapi.TemplateBuildStatusReady
	template.UpdatedAt = time.Now().UTC()
	_, err = a.management.UpsertTemplate(template, nil, localBuildLogs(message))
	return err
}

func (a *App) defaultGetTemplateBuildInfo(ctx context.Context, templateID string, buildID string, req TemplateBuildInfoRequest) (e2bapi.TemplateBuildInfo, error) {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return e2bapi.TemplateBuildInfo{}, err
	}
	if build, ok := a.management.TemplateBuild(template, buildID); ok {
		logs := filterBuildLogs(build.Logs, req.Level)
		logs = offsetBuildLogs(logs, req.LogsOffset)
		logs = limitBuildLogs(logs, req.Limit)
		return e2bapi.TemplateBuildInfo{
			BuildID:    buildIDOrStable(build.Template, buildID),
			LogEntries: logs,
			Logs:       buildLogMessages(logs),
			Status:     templateBuildStatus(build.Template),
			TemplateID: template.TemplateID,
		}, nil
	}
	if !templateBuildMatches(template, buildID) {
		return e2bapi.TemplateBuildInfo{}, gatewayError(http.StatusNotFound, "template build %s not found", buildID)
	}

	logs := filterBuildLogs(a.management.TemplateBuildLogs(template.TemplateID), req.Level)
	logs = offsetBuildLogs(logs, req.LogsOffset)
	logs = limitBuildLogs(logs, req.Limit)
	return e2bapi.TemplateBuildInfo{
		BuildID:    buildIDOrStable(template, buildID),
		LogEntries: logs,
		Logs:       buildLogMessages(logs),
		Status:     templateBuildStatus(template),
		TemplateID: template.TemplateID,
	}, nil
}

func (a *App) defaultGetTemplateBuildLogs(ctx context.Context, templateID string, buildID string, req TemplateBuildLogsRequest) (e2bapi.TemplateBuildLogsResponse, error) {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return e2bapi.TemplateBuildLogsResponse{}, err
	}
	if build, ok := a.management.TemplateBuild(template, buildID); ok {
		logs := filterBuildLogs(build.Logs, req.Level)
		logs = cursorBuildLogs(logs, req.Cursor)
		logs = limitBuildLogs(logs, req.Limit)
		if req.Direction != nil && *req.Direction == e2bapi.LogsDirectionBackward {
			reverseBuildLogs(logs)
		}
		return e2bapi.TemplateBuildLogsResponse{Logs: logs}, nil
	}
	if !templateBuildMatches(template, buildID) {
		return e2bapi.TemplateBuildLogsResponse{}, gatewayError(http.StatusNotFound, "template build %s not found", buildID)
	}

	logs := filterBuildLogs(a.management.TemplateBuildLogs(template.TemplateID), req.Level)
	logs = cursorBuildLogs(logs, req.Cursor)
	logs = limitBuildLogs(logs, req.Limit)
	if req.Direction != nil && *req.Direction == e2bapi.LogsDirectionBackward {
		reverseBuildLogs(logs)
	}
	return e2bapi.TemplateBuildLogsResponse{Logs: logs}, nil
}

func (a *App) defaultGetTemplateFileUpload(ctx context.Context, templateID string, hash string) (e2bapi.TemplateBuildFileUpload, error) {
	if strings.TrimSpace(hash) == "" {
		return e2bapi.TemplateBuildFileUpload{}, gatewayError(http.StatusBadRequest, "hash is required")
	}
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return e2bapi.TemplateBuildFileUpload{}, err
	}

	token, err := a.management.CreateTemplateFileUpload(template.TemplateID, hash)
	if err != nil {
		return e2bapi.TemplateBuildFileUpload{}, err
	}
	uploadURL, err := templateFileUploadURL(ctx, template.TemplateID, hash, token)
	if err != nil {
		return e2bapi.TemplateBuildFileUpload{}, err
	}
	return e2bapi.TemplateBuildFileUpload{
		Present: a.management.TemplateFilePresent(template.TemplateID, hash),
		Url:     &uploadURL,
	}, nil
}

func templateFileUploadURL(ctx context.Context, templateID string, hash string, token string) (string, error) {
	base, ok := inboundRequestBaseURL(ctx)
	if !ok {
		return "", gatewayError(http.StatusInternalServerError, "cannot determine gateway upload URL")
	}

	query := url.Values{}
	query.Set("token", token)
	return base +
		"/_e2b/template-files/" +
		url.PathEscape(templateID) +
		"/" +
		url.PathEscape(hash) +
		"?" +
		query.Encode(), nil
}

func (a *App) defaultAssignTemplateTags(ctx context.Context, req e2bapi.AssignTemplateTagsRequest) (e2bapi.AssignedTemplateTags, error) {
	if strings.TrimSpace(req.Target) == "" {
		return e2bapi.AssignedTemplateTags{}, gatewayError(http.StatusBadRequest, "target is required")
	}
	tags := normalizedTags(req.Tags)
	if len(tags) == 0 {
		return e2bapi.AssignedTemplateTags{}, gatewayError(http.StatusBadRequest, "tags are required")
	}

	template, err := a.findTemplateByTaggedName(ctx, req.Target)
	if err != nil {
		return e2bapi.AssignedTemplateTags{}, err
	}
	return a.management.AssignTemplateTags(template, tags)
}

func (a *App) defaultDeleteTemplateTags(ctx context.Context, req e2bapi.DeleteTemplateTagsRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return gatewayError(http.StatusBadRequest, "name is required")
	}
	tags := normalizedTags(req.Tags)
	if len(tags) == 0 {
		return gatewayError(http.StatusBadRequest, "tags are required")
	}

	template, err := a.findTemplateByTaggedName(ctx, req.Name)
	if err != nil {
		return err
	}
	if _, err := a.management.UpsertTemplate(template, nil, nil); err != nil {
		return err
	}
	deleted, err := a.management.DeleteTemplateTags(template.TemplateID, tags)
	if err != nil {
		return err
	}
	if !deleted {
		return gatewayError(http.StatusNotFound, "template %s not found", req.Name)
	}
	return nil
}

func (a *App) defaultListTemplateTags(ctx context.Context, templateID string) ([]e2bapi.TemplateTag, error) {
	template, err := a.findTemplate(ctx, templateID)
	if err != nil {
		return nil, err
	}

	if tags := a.management.ListTemplateTags(template.TemplateID); len(tags) > 0 {
		return tags, nil
	}

	tag := dockerImageTag(template.ImageRef)
	if tag == "" {
		return []e2bapi.TemplateTag{}, nil
	}

	return []e2bapi.TemplateTag{
		{
			BuildID:   stableTemplateBuildUUID(template.TemplateID, template.BuildID),
			CreatedAt: template.CreatedAt,
			Tag:       tag,
		},
	}, nil
}

func (a *App) defaultListNodes(ctx context.Context) ([]e2bapi.Node, error) {
	return []e2bapi.Node{a.localNode()}, nil
}

func (a *App) defaultGetNode(ctx context.Context, nodeID string, req NodeDetailRequest) (e2bapi.NodeDetail, error) {
	if strings.TrimSpace(nodeID) != localNodeID {
		return e2bapi.NodeDetail{}, gatewayError(http.StatusNotFound, "node %s not found", nodeID)
	}

	node := a.localNode()
	cachedBuilds := []string{}
	templates, err := a.callbacks.ListTemplates(ctx, e2bapi.GetTemplatesParams{})
	if err == nil {
		for _, template := range templates {
			if template.BuildID != "" {
				cachedBuilds = appendUniqueStrings(cachedBuilds, template.BuildID)
			}
		}
	}

	return e2bapi.NodeDetail{
		CachedBuilds:      cachedBuilds,
		ClusterID:         node.ClusterID,
		Commit:            node.Commit,
		CreateFails:       node.CreateFails,
		CreateSuccesses:   node.CreateSuccesses,
		Id:                node.Id,
		MachineInfo:       node.MachineInfo,
		Metrics:           node.Metrics,
		SandboxCount:      node.SandboxCount,
		ServiceInstanceID: node.ServiceInstanceID,
		Status:            node.Status,
		Version:           node.Version,
	}, nil
}

func (a *App) defaultSetNodeStatus(ctx context.Context, nodeID string, req e2bapi.NodeStatusChange) error {
	if strings.TrimSpace(nodeID) != localNodeID {
		return gatewayError(http.StatusNotFound, "node %s not found", nodeID)
	}
	if !validNodeStatus(req.Status) {
		return gatewayError(http.StatusBadRequest, "unsupported node status %q", req.Status)
	}
	return a.management.SetNodeStatus(localNodeID, req.Status)
}

func (a *App) defaultListTeams(ctx context.Context) ([]e2bapi.Team, error) {
	return []e2bapi.Team{
		{
			ApiKey:    "local",
			IsDefault: true,
			Name:      "local",
			TeamID:    localTeamID,
		},
	}, nil
}

func (a *App) defaultGetTeamMetrics(ctx context.Context, teamID string, req TeamMetricsRequest) ([]e2bapi.TeamMetric, error) {
	if err := validateTeamMetricsRequest(req); err != nil {
		return nil, err
	}

	metric := a.teamMetricSnapshot(time.Now().UTC(), req)
	if !teamMetricMatchesRange(metric, req) {
		return []e2bapi.TeamMetric{}, nil
	}
	return []e2bapi.TeamMetric{metric}, nil
}

func (a *App) defaultGetTeamMetricMax(ctx context.Context, teamID string, req TeamMetricMaxRequest) (e2bapi.MaxTeamMetric, error) {
	if err := validateTeamMetricsRequest(TeamMetricsRequest{Start: req.Start, End: req.End}); err != nil {
		return e2bapi.MaxTeamMetric{}, err
	}

	metric := a.teamMetricSnapshot(time.Now().UTC(), TeamMetricsRequest{Start: req.Start, End: req.End})
	value := float32(0)
	switch req.Metric {
	case e2bapi.ConcurrentSandboxes:
		value = float32(metric.ConcurrentSandboxes)
	case e2bapi.SandboxStartRate:
		value = metric.SandboxStartRate
	default:
		return e2bapi.MaxTeamMetric{}, gatewayError(http.StatusBadRequest, "unsupported team metric %q", req.Metric)
	}

	return e2bapi.MaxTeamMetric{
		Timestamp:     metric.Timestamp,
		TimestampUnix: metric.TimestampUnix,
		Value:         value,
	}, nil
}

func (a *App) defaultKillTeamSandboxes(ctx context.Context, teamID string) (e2bapi.AdminSandboxKillResult, error) {
	result := e2bapi.AdminSandboxKillResult{}
	for _, record := range a.store.List() {
		if err := a.defaultKillSandbox(ctx, record.ID); err != nil {
			result.FailedCount++
			a.logger.Printf("admin sandbox kill failed team_id=%s sandbox_id=%s error=%v", teamID, record.ID, err)
			continue
		}
		result.KilledCount++
	}
	return result, nil
}

func (a *App) defaultCancelTeamBuilds(ctx context.Context, teamID string) (e2bapi.AdminBuildCancelResult, error) {
	if a.builds == nil {
		return e2bapi.AdminBuildCancelResult{}, nil
	}
	return a.builds.cancelTeamBuilds(teamID), nil
}

func (a *App) findTemplateByTaggedName(ctx context.Context, value string) (GatewayTemplate, error) {
	if template, err := a.findTemplate(ctx, value); err == nil {
		return template, nil
	}

	name, _ := splitTemplateNameAndTags(value, nil)
	if name != value {
		return a.findTemplate(ctx, name)
	}
	return GatewayTemplate{}, gatewayError(http.StatusNotFound, "template %s not found", value)
}

func (a *App) findTemplate(ctx context.Context, templateID string) (GatewayTemplate, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return GatewayTemplate{}, gatewayError(http.StatusNotFound, "template not found")
	}

	listTemplates := a.defaultListTemplates
	if a.callbacks.ListTemplates != nil {
		listTemplates = a.callbacks.ListTemplates
	}

	templates, err := listTemplates(ctx, e2bapi.GetTemplatesParams{})
	if err != nil {
		return GatewayTemplate{}, err
	}

	for _, template := range templates {
		if templateMatchesID(template, templateID) {
			return template, nil
		}
	}
	return GatewayTemplate{}, gatewayError(http.StatusNotFound, "template %s not found", templateID)
}

func (a *App) defaultListVolumes(ctx context.Context) ([]e2bapi.Volume, error) {
	volumeRuntime, err := a.defaultVolumeRuntime()
	if err != nil {
		return nil, err
	}

	volumes, err := volumeRuntime.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]e2bapi.Volume, 0, len(volumes))
	for _, volume := range volumes {
		result = append(result, apiVolume(volume))
	}
	return result, nil
}

func (a *App) defaultCreateVolume(ctx context.Context, req e2bapi.NewVolume) (e2bapi.VolumeAndToken, error) {
	volumeRuntime, err := a.defaultVolumeRuntime()
	if err != nil {
		return e2bapi.VolumeAndToken{}, err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return e2bapi.VolumeAndToken{}, gatewayError(http.StatusBadRequest, "name is required")
	}

	volume, err := volumeRuntime.CreateVolume(ctx, name)
	if err != nil {
		return e2bapi.VolumeAndToken{}, err
	}
	return apiVolumeAndToken(volume), nil
}

func (a *App) defaultGetVolume(ctx context.Context, volumeID string) (e2bapi.VolumeAndToken, error) {
	volumeRuntime, err := a.defaultVolumeRuntime()
	if err != nil {
		return e2bapi.VolumeAndToken{}, err
	}

	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return e2bapi.VolumeAndToken{}, gatewayError(http.StatusNotFound, "volume not found")
	}

	volume, err := volumeRuntime.GetVolume(ctx, volumeID)
	if err != nil {
		return e2bapi.VolumeAndToken{}, gatewayError(volumeErrorStatus(err), "%s", err.Error())
	}
	return apiVolumeAndToken(volume), nil
}

func (a *App) defaultDeleteVolume(ctx context.Context, volumeID string) error {
	volumeRuntime, err := a.defaultVolumeRuntime()
	if err != nil {
		return err
	}

	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return gatewayError(http.StatusNotFound, "volume not found")
	}

	deleted, err := volumeRuntime.DeleteVolume(ctx, volumeID)
	if err != nil {
		return gatewayError(volumeErrorStatus(err), "%s", err.Error())
	}
	if !deleted {
		return gatewayError(http.StatusNotFound, "volume %s not found", volumeID)
	}
	return nil
}

func (a *App) defaultVolumeRuntime() (VolumeRuntime, error) {
	volumeRuntime, ok := a.runtime.(VolumeRuntime)
	if !ok {
		return nil, gatewayError(http.StatusNotImplemented, "volumes are not supported by this runtime")
	}
	return volumeRuntime, nil
}

func stringSlicePtrLen(values *[]string) int {
	if values == nil {
		return 0
	}
	return len(*values)
}

func validateMetricsRequest(req SandboxMetricsRequest) error {
	if req.Start != nil && *req.Start < 0 {
		return gatewayError(http.StatusBadRequest, "start must be greater than or equal to 0")
	}
	if req.End != nil && *req.End < 0 {
		return gatewayError(http.StatusBadRequest, "end must be greater than or equal to 0")
	}
	if req.Start != nil && req.End != nil && *req.Start > *req.End {
		return gatewayError(http.StatusBadRequest, "start must be less than or equal to end")
	}
	return nil
}

func validateTeamMetricsRequest(req TeamMetricsRequest) error {
	if req.Start != nil && *req.Start < 0 {
		return gatewayError(http.StatusBadRequest, "start must be greater than or equal to 0")
	}
	if req.End != nil && *req.End < 0 {
		return gatewayError(http.StatusBadRequest, "end must be greater than or equal to 0")
	}
	if req.Start != nil && req.End != nil && *req.Start > *req.End {
		return gatewayError(http.StatusBadRequest, "start must be less than or equal to end")
	}
	return nil
}

func sandboxMetricFromRecord(record SandboxRecord, timestamp time.Time) e2bapi.SandboxMetric {
	timestamp = timestamp.UTC()
	return e2bapi.SandboxMetric{
		CpuCount:      sandboxCPUCount(record),
		CpuUsedPct:    0,
		DiskTotal:     int64(sandboxDiskSizeMB(record)) * 1024 * 1024,
		DiskUsed:      0,
		MemCache:      0,
		MemTotal:      int64(sandboxMemoryMB(record)) * 1024 * 1024,
		MemUsed:       0,
		Timestamp:     timestamp,
		TimestampUnix: timestamp.Unix(),
	}
}

func SandboxMetricFromRecord(record SandboxRecord, timestamp time.Time) e2bapi.SandboxMetric {
	return sandboxMetricFromRecord(record, timestamp)
}

func metricMatchesRange(metric e2bapi.SandboxMetric, req SandboxMetricsRequest) bool {
	if req.Start != nil && metric.TimestampUnix < *req.Start {
		return false
	}
	if req.End != nil && metric.TimestampUnix > *req.End {
		return false
	}
	return true
}

func MetricMatchesRange(metric e2bapi.SandboxMetric, req SandboxMetricsRequest) bool {
	return metricMatchesRange(metric, req)
}

func (a *App) teamMetricSnapshot(timestamp time.Time, req TeamMetricsRequest) e2bapi.TeamMetric {
	timestamp = timestamp.UTC()
	windowEnd := timestamp.Unix()
	if req.End != nil {
		windowEnd = *req.End
	}
	windowStart := windowEnd - 60
	if req.Start != nil {
		windowStart = *req.Start
	}
	if windowStart > windowEnd {
		windowStart = windowEnd
	}

	running := int32(0)
	started := 0
	for _, record := range a.store.List() {
		if apiSandboxState(record.State) == e2bapi.Running {
			running++
		}
		createdAt := record.CreatedAt.Unix()
		if createdAt >= windowStart && createdAt <= windowEnd {
			started++
		}
	}

	duration := windowEnd - windowStart
	if duration <= 0 {
		duration = 1
	}

	return e2bapi.TeamMetric{
		ConcurrentSandboxes: running,
		SandboxStartRate:    float32(started) / float32(duration),
		Timestamp:           timestamp,
		TimestampUnix:       timestamp.Unix(),
	}
}

func teamMetricMatchesRange(metric e2bapi.TeamMetric, req TeamMetricsRequest) bool {
	if req.Start != nil && metric.TimestampUnix < *req.Start {
		return false
	}
	if req.End != nil && metric.TimestampUnix > *req.End {
		return false
	}
	return true
}

func (a *App) localNode() e2bapi.Node {
	records := a.store.List()
	running := uint32(0)
	for _, record := range records {
		if apiSandboxState(record.State) == e2bapi.Running {
			running++
		}
	}

	cpuCount := uint32(goruntime.NumCPU())
	return e2bapi.Node{
		ClusterID:            localTeamID,
		Commit:               "local",
		CreateFails:          0,
		CreateSuccesses:      uint64(len(records)),
		Id:                   localNodeID,
		MachineInfo:          localMachineInfo(),
		Metrics:              localNodeMetrics(cpuCount, running),
		SandboxCount:         running,
		SandboxStartingCount: 0,
		ServiceInstanceID:    "e2b-local",
		Status:               a.management.NodeStatus(localNodeID),
		Version:              defaultEnvdVersion,
	}
}

func localMachineInfo() e2bapi.MachineInfo {
	return e2bapi.MachineInfo{
		CpuArchitecture: goruntime.GOARCH,
		CpuFamily:       goruntime.GOOS,
		CpuModel:        "local",
		CpuModelName:    "local",
	}
}

func localNodeMetrics(cpuCount uint32, running uint32) e2bapi.NodeMetrics {
	return e2bapi.NodeMetrics{
		AllocatedCPU:         running,
		AllocatedMemoryBytes: uint64(running) * uint64(512*1024*1024),
		CpuCount:             cpuCount,
		CpuPercent:           0,
		Disks:                []e2bapi.DiskMetrics{},
		MemoryTotalBytes:     0,
		MemoryUsedBytes:      0,
	}
}

func validNodeStatus(status e2bapi.NodeStatus) bool {
	switch status {
	case e2bapi.NodeStatusConnecting,
		e2bapi.NodeStatusDraining,
		e2bapi.NodeStatusReady,
		e2bapi.NodeStatusStandby,
		e2bapi.NodeStatusUnhealthy:
		return true
	default:
		return false
	}
}

func templateWithBuilds(template GatewayTemplate) e2bapi.TemplateWithBuilds {
	createdAt := template.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := template.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	return e2bapi.TemplateWithBuilds{
		Aliases:       append([]string(nil), template.Aliases...),
		Builds:        []e2bapi.TemplateBuild{templateBuild(template)},
		CreatedAt:     createdAt,
		LastSpawnedAt: template.LastSpawnedAt,
		Names:         append([]string(nil), template.Names...),
		Public:        template.Public,
		SpawnCount:    template.SpawnCount,
		TemplateID:    template.TemplateID,
		UpdatedAt:     updatedAt,
	}
}

func templateBuildRecords(records []GatewayTemplateBuildRecord) []e2bapi.TemplateBuild {
	builds := make([]e2bapi.TemplateBuild, 0, len(records))
	for _, record := range records {
		builds = append(builds, templateBuild(record.Template))
	}
	return builds
}

func templateBuild(template GatewayTemplate) e2bapi.TemplateBuild {
	createdAt := template.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := template.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	finishedAt := updatedAt
	diskSizeMB := template.DiskSizeMB
	envdVersion := template.EnvdVersion

	return e2bapi.TemplateBuild{
		BuildID:     stableTemplateBuildUUID(template.TemplateID, template.BuildID),
		CpuCount:    template.CpuCount,
		CreatedAt:   createdAt,
		DiskSizeMB:  &diskSizeMB,
		EnvdVersion: &envdVersion,
		FinishedAt:  &finishedAt,
		MemoryMB:    template.MemoryMB,
		Status:      templateBuildStatus(template),
		UpdatedAt:   updatedAt,
	}
}

func (a *App) newManagedTemplate(name string, alias *string, requestTags *[]string, cpuCount *e2bapi.CPUCount, memoryMB *e2bapi.MemoryMB) (GatewayTemplate, []string) {
	name, tags := splitTemplateNameAndTags(name, requestTags)
	templateID := localTemplateID(name)
	now := time.Now().UTC()
	buildID := uuid.NewString()

	cpu := e2bapi.CPUCount(1)
	if cpuCount != nil && *cpuCount > 0 {
		cpu = *cpuCount
	}
	memory := e2bapi.MemoryMB(512)
	if memoryMB != nil && *memoryMB > 0 {
		memory = *memoryMB
	}

	names := []string{name}
	aliases := []string{}
	if alias != nil && strings.TrimSpace(*alias) != "" {
		aliases = appendUniqueStrings(aliases, strings.TrimSpace(*alias))
		names = appendUniqueStrings(names, strings.TrimSpace(*alias))
	}

	return GatewayTemplate{
		Template: e2bapi.Template{
			Aliases:     aliases,
			BuildCount:  1,
			BuildID:     buildID,
			BuildStatus: e2bapi.TemplateBuildStatusReady,
			CpuCount:    cpu,
			CreatedAt:   now,
			DiskSizeMB:  0,
			EnvdVersion: defaultEnvdVersion,
			MemoryMB:    memory,
			Names:       names,
			Public:      false,
			SpawnCount:  0,
			TemplateID:  templateID,
			UpdatedAt:   now,
		},
	}, tags
}

func templateLegacy(template GatewayTemplate) e2bapi.TemplateLegacy {
	createdAt := template.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := template.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	return e2bapi.TemplateLegacy{
		Aliases:       append([]string(nil), template.Aliases...),
		BuildCount:    template.BuildCount,
		BuildID:       template.BuildID,
		CpuCount:      template.CpuCount,
		CreatedAt:     createdAt,
		CreatedBy:     template.CreatedBy,
		DiskSizeMB:    template.DiskSizeMB,
		EnvdVersion:   template.EnvdVersion,
		LastSpawnedAt: template.LastSpawnedAt,
		MemoryMB:      template.MemoryMB,
		Public:        template.Public,
		SpawnCount:    template.SpawnCount,
		TemplateID:    template.TemplateID,
		UpdatedAt:     updatedAt,
	}
}

func templateRequestResponseV3(template GatewayTemplate, tags []string) e2bapi.TemplateRequestResponseV3 {
	return e2bapi.TemplateRequestResponseV3{
		Aliases:    append([]string(nil), template.Aliases...),
		BuildID:    template.BuildID,
		Names:      append([]string(nil), template.Names...),
		Public:     template.Public,
		Tags:       normalizedTags(tags),
		TemplateID: template.TemplateID,
	}
}

func splitTemplateNameAndTags(value string, requestTags *[]string) (string, []string) {
	value = strings.TrimSpace(value)
	tags := []string{}
	if index := strings.LastIndex(value, ":"); index > 0 && index < len(value)-1 && !strings.Contains(value[index+1:], "/") {
		tags = append(tags, value[index+1:])
		value = value[:index]
	}
	if requestTags != nil {
		tags = append(tags, (*requestTags)...)
	}
	if value == "" {
		value = "template-" + uuid.NewString()[:8]
	}
	return value, normalizedTags(tags)
}

func localTemplateID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "template-" + uuid.NewString()[:8]
	}

	var builder strings.Builder
	for _, ch := range strings.ToLower(name) {
		switch {
		case ch >= 'a' && ch <= 'z':
			builder.WriteRune(ch)
		case ch >= '0' && ch <= '9':
			builder.WriteRune(ch)
		case ch == '-' || ch == '_' || ch == '.':
			builder.WriteRune(ch)
		default:
			builder.WriteRune('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "template-" + uuid.NewString()[:8]
	}
	return result
}

func localBuildLogs(message string) []e2bapi.BuildLogEntry {
	return []e2bapi.BuildLogEntry{
		{
			Level:     e2bapi.LogLevelInfo,
			Message:   message,
			Timestamp: time.Now().UTC(),
		},
	}
}

func localBuildErrorLogs(message string) []e2bapi.BuildLogEntry {
	return []e2bapi.BuildLogEntry{
		{
			Level:     e2bapi.LogLevelError,
			Message:   message,
			Timestamp: time.Now().UTC(),
		},
	}
}

func appendBuildLogs(groups ...[]e2bapi.BuildLogEntry) []e2bapi.BuildLogEntry {
	var total int
	for _, group := range groups {
		total += len(group)
	}
	if total == 0 {
		return nil
	}

	result := make([]e2bapi.BuildLogEntry, 0, total)
	for _, group := range groups {
		result = append(result, group...)
	}
	return result
}

func templateBuildStatus(template GatewayTemplate) e2bapi.TemplateBuildStatus {
	if template.BuildStatus == "" {
		return e2bapi.TemplateBuildStatusReady
	}
	return template.BuildStatus
}

func templateBuildMatches(template GatewayTemplate, buildID string) bool {
	buildID = strings.TrimSpace(buildID)
	if buildID == "" {
		return false
	}
	return buildID == template.BuildID || buildID == stableTemplateBuildUUID(template.TemplateID, template.BuildID).String()
}

func buildIDOrStable(template GatewayTemplate, requestedBuildID string) string {
	if strings.TrimSpace(requestedBuildID) != "" {
		return requestedBuildID
	}
	if template.BuildID != "" {
		return template.BuildID
	}
	return stableTemplateBuildUUID(template.TemplateID, template.BuildID).String()
}

func filterBuildLogs(logs []e2bapi.BuildLogEntry, level *e2bapi.LogLevel) []e2bapi.BuildLogEntry {
	if level == nil {
		return append([]e2bapi.BuildLogEntry(nil), logs...)
	}
	result := make([]e2bapi.BuildLogEntry, 0, len(logs))
	for _, entry := range logs {
		if entry.Level == *level {
			result = append(result, entry)
		}
	}
	return result
}

func offsetBuildLogs(logs []e2bapi.BuildLogEntry, offset *int32) []e2bapi.BuildLogEntry {
	if offset == nil || *offset <= 0 {
		return logs
	}
	start := int(*offset)
	if start >= len(logs) {
		return []e2bapi.BuildLogEntry{}
	}
	return logs[start:]
}

func cursorBuildLogs(logs []e2bapi.BuildLogEntry, cursor *int64) []e2bapi.BuildLogEntry {
	if cursor == nil || *cursor <= 0 {
		return logs
	}
	result := make([]e2bapi.BuildLogEntry, 0, len(logs))
	for _, entry := range logs {
		if entry.Timestamp.UnixMilli() >= *cursor {
			result = append(result, entry)
		}
	}
	return result
}

func limitBuildLogs(logs []e2bapi.BuildLogEntry, limit *int32) []e2bapi.BuildLogEntry {
	if limit == nil || *limit <= 0 || int(*limit) >= len(logs) {
		return logs
	}
	return logs[:int(*limit)]
}

func reverseBuildLogs(logs []e2bapi.BuildLogEntry) {
	for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
		logs[i], logs[j] = logs[j], logs[i]
	}
}

func buildLogMessages(logs []e2bapi.BuildLogEntry) []string {
	messages := make([]string, 0, len(logs))
	for _, entry := range logs {
		messages = append(messages, entry.Message)
	}
	return messages
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values)+len(additions))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func AppendUniqueStrings(values []string, additions ...string) []string {
	return appendUniqueStrings(values, additions...)
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func StringPtrValue(value *string) string {
	return stringPtrValue(value)
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func BoolPtrValue(value *bool) bool {
	return boolPtrValue(value)
}

func stableTemplateBuildUUID(templateID string, buildID string) uuid.UUID {
	value := strings.TrimSpace(buildID)
	if value != "" {
		if parsed, err := uuid.Parse(value); err == nil {
			return parsed
		}
	}
	if value == "" {
		value = strings.TrimSpace(templateID)
	}

	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("e2b-local/template-build/"+strings.TrimSpace(templateID)+"/"+value))
}

func templateMatchesID(template GatewayTemplate, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if template.TemplateID == id || template.ImageRef == id {
		return true
	}
	if template.ImageRef != "" && (shortDockerImageName(template.ImageRef) == id || dockerTemplateName(template.ImageRef) == id) {
		return true
	}
	for _, name := range template.Names {
		if name == id {
			return true
		}
	}
	for _, alias := range template.Aliases {
		if alias == id {
			return true
		}
	}
	return false
}

func dockerImageTag(imageRef string) string {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" || strings.Contains(imageRef, "@") {
		return ""
	}

	lastSlash := strings.LastIndex(imageRef, "/")
	tagIndex := strings.LastIndex(imageRef, ":")
	if tagIndex <= lastSlash || tagIndex == len(imageRef)-1 {
		return ""
	}
	return imageRef[tagIndex+1:]
}

func parseMetadataFilter(value *string) (map[string]string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, nil
	}

	values, err := url.ParseQuery(*value)
	if err != nil {
		return nil, gatewayError(http.StatusBadRequest, "invalid metadata filter: %v", err)
	}

	filter := make(map[string]string, len(values))
	for key, items := range values {
		if key == "" {
			continue
		}
		if len(items) == 0 {
			filter[key] = ""
			continue
		}
		filter[key] = items[len(items)-1]
	}
	return filter, nil
}

func metadataMatches(metadata map[string]string, filter map[string]string) bool {
	if len(filter) == 0 {
		return true
	}
	for key, value := range filter {
		if metadata[key] != value {
			return false
		}
	}
	return true
}

func sandboxStateFilter(states []e2bapi.SandboxState) map[string]bool {
	if len(states) == 0 {
		return nil
	}
	filter := make(map[string]bool, len(states))
	for _, state := range states {
		filter[string(state)] = true
	}
	return filter
}

func sandboxMetadata(metadata *e2bapi.SandboxMetadata) map[string]string {
	if metadata == nil {
		return nil
	}
	return copyStringMap(map[string]string(*metadata))
}

func sandboxEnvVars(envVars *e2bapi.EnvVars) map[string]string {
	if envVars == nil {
		return nil
	}
	return copyStringMap(map[string]string(*envVars))
}

func sandboxVolumeMounts(volumeMounts *[]e2bapi.SandboxVolumeMount) []VolumeMount {
	if volumeMounts == nil || len(*volumeMounts) == 0 {
		return nil
	}

	result := make([]VolumeMount, 0, len(*volumeMounts))
	for _, volumeMount := range *volumeMounts {
		name := strings.TrimSpace(volumeMount.Name)
		path := strings.TrimSpace(volumeMount.Path)
		if name == "" && path == "" {
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

func requestTimeout(timeout *int32) int32 {
	if timeout == nil || *timeout <= 0 {
		return defaultSandboxTimeoutSeconds
	}
	return *timeout
}

func paginationLimit(limit *int32) (int, error) {
	if limit == nil {
		return maxSandboxListLimit, nil
	}
	if *limit <= 0 {
		return 0, gatewayError(http.StatusBadRequest, "limit must be positive")
	}
	if *limit > maxSandboxListLimit {
		return maxSandboxListLimit, nil
	}
	return int(*limit), nil
}

func intHeader(value int) string {
	return strconv.Itoa(value)
}
