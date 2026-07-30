package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"

	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

type sandboxAPIResponse struct {
	e2bapi.Sandbox
	EnvdURL      string        `json:"envdURL,omitempty"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
}

type listedSandboxAPIResponse struct {
	e2bapi.ListedSandbox
	EnvdURL string `json:"envdURL,omitempty"`
}

func (a *App) GetHealth(c *gin.Context) {
	if err := a.callbacks.Health(a.callbackContext(c)); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.String(http.StatusOK, "Health check successful")
}

func (a *App) GetNodes(c *gin.Context) {
	nodes, err := a.callbacks.ListNodes(a.callbackContext(c))
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, nodes)
}

func (a *App) GetNodesNodeID(c *gin.Context, nodeID e2bapi.NodeID, params e2bapi.GetNodesNodeIDParams) {
	node, err := a.callbacks.GetNode(a.callbackContext(c), nodeID, NodeDetailRequest{
		ClusterID: params.ClusterID,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, node)
}

func (a *App) PostNodesNodeID(c *gin.Context, nodeID e2bapi.NodeID) {
	var req e2bapi.NodeStatusChange
	if err := bindOptionalJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.callbacks.SetNodeStatus(a.callbackContext(c), nodeID, req); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) PostSandboxes(c *gin.Context) {
	var req e2bapi.NewSandbox
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	record, err := a.callbacks.CreateSandbox(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusCreated, a.apiSandboxResponse(record))
}

func (a *App) GetSandboxes(c *gin.Context, params e2bapi.GetSandboxesParams) {
	metadata, err := parseMetadataFilter(params.Metadata)
	if err != nil {
		writeGatewayError(c, err, http.StatusBadRequest)
		return
	}

	result, err := a.callbacks.ListSandboxes(a.callbackContext(c), SandboxListRequest{
		Metadata: metadata,
		States:   []e2bapi.SandboxState{e2bapi.Running},
		Limit:    maxSandboxListLimit,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, a.apiListedSandboxes(result.Items))
}

func (a *App) GetV2Sandboxes(c *gin.Context, params e2bapi.GetV2SandboxesParams) {
	metadata, err := parseMetadataFilter(params.Metadata)
	if err != nil {
		writeGatewayError(c, err, http.StatusBadRequest)
		return
	}

	states := []e2bapi.SandboxState{e2bapi.Running, e2bapi.Paused}
	if params.State != nil {
		states = *params.State
	}

	limit, err := paginationLimit(params.Limit)
	if err != nil {
		writeGatewayError(c, err, http.StatusBadRequest)
		return
	}

	nextToken := ""
	if params.NextToken != nil {
		nextToken = *params.NextToken
	}

	result, err := a.callbacks.ListSandboxes(a.callbackContext(c), SandboxListRequest{
		Metadata:  metadata,
		States:    states,
		Limit:     limit,
		NextToken: nextToken,
		V2:        true,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	if result.NextToken != "" {
		c.Header("X-Next-Token", result.NextToken)
	}
	c.Header("X-Total-Running", intHeader(result.TotalRunning))
	c.JSON(http.StatusOK, a.apiListedSandboxes(result.Items))
}

func (a *App) GetSandboxesMetrics(c *gin.Context, params e2bapi.GetSandboxesMetricsParams) {
	result, err := a.callbacks.ListSandboxMetrics(a.callbackContext(c), params.SandboxIds)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (a *App) GetSandboxesSandboxID(c *gin.Context, sandboxID e2bapi.SandboxID) {
	record, err := a.callbacks.GetSandbox(a.callbackContext(c), sandboxID)
	if err != nil {
		writeGatewayError(c, err, http.StatusNotFound)
		return
	}

	c.JSON(http.StatusOK, a.apiSandboxDetailResponse(record))
}

func (a *App) GetSandboxesSandboxIDMetrics(c *gin.Context, sandboxID e2bapi.SandboxID, params e2bapi.GetSandboxesSandboxIDMetricsParams) {
	metrics, err := a.callbacks.GetSandboxMetrics(a.callbackContext(c), sandboxID, SandboxMetricsRequest{
		Start: params.Start,
		End:   params.End,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, metrics)
}

func (a *App) DeleteSandboxesSandboxID(c *gin.Context, sandboxID e2bapi.SandboxID) {
	if err := a.callbacks.KillSandbox(a.callbackContext(c), sandboxID); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) PostSandboxesSandboxIDPause(c *gin.Context, sandboxID e2bapi.SandboxID) {
	_, err := a.callbacks.PauseSandbox(a.callbackContext(c), sandboxID)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) PostSandboxesSandboxIDResume(c *gin.Context, sandboxID e2bapi.SandboxID) {
	var req e2bapi.ResumedSandbox
	if err := bindOptionalJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	record, err := a.callbacks.ResumeSandbox(a.callbackContext(c), sandboxID, req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusCreated, a.apiSandboxResponse(record))
}

func (a *App) PostSandboxesSandboxIDConnect(c *gin.Context, sandboxID e2bapi.SandboxID) {
	var body struct {
		Timeout *int32 `json:"timeout"`
	}
	if err := bindJSON(c, &body); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if body.Timeout == nil {
		writeError(c, http.StatusBadRequest, "timeout is required")
		return
	}
	req := e2bapi.ConnectSandbox{Timeout: *body.Timeout}

	record, resumed, err := a.callbacks.ConnectSandbox(a.callbackContext(c), sandboxID, req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	status := http.StatusOK
	if resumed {
		status = http.StatusCreated
	}
	c.JSON(status, a.apiSandboxResponse(record))
}

func (a *App) PostSandboxesSandboxIDTimeout(c *gin.Context, sandboxID e2bapi.SandboxID) {
	var req e2bapi.PostSandboxesSandboxIDTimeoutJSONBody
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.callbacks.SetSandboxTimeout(a.callbackContext(c), sandboxID, req); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) PostSandboxesSandboxIDRefreshes(c *gin.Context, sandboxID e2bapi.SandboxID) {
	var req e2bapi.PostSandboxesSandboxIDRefreshesJSONBody
	if err := bindOptionalJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.callbacks.RefreshSandbox(a.callbackContext(c), sandboxID, req); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) PostSandboxesSandboxIDSnapshots(c *gin.Context, sandboxID e2bapi.SandboxID) {
	var req e2bapi.PostSandboxesSandboxIDSnapshotsJSONBody
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	snapshot, err := a.callbacks.CreateSandboxSnapshot(a.callbackContext(c), sandboxID, req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusCreated, snapshot)
}

func (a *App) GetSnapshots(c *gin.Context, params e2bapi.GetSnapshotsParams) {
	limit, err := paginationLimit(params.Limit)
	if err != nil {
		writeGatewayError(c, err, http.StatusBadRequest)
		return
	}

	req := SnapshotListRequest{Limit: limit}
	if params.SandboxID != nil {
		req.SandboxID = *params.SandboxID
	}
	if params.NextToken != nil {
		req.NextToken = string(*params.NextToken)
	}

	snapshots, err := a.callbacks.ListSnapshots(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, snapshots)
}

func (a *App) GetSandboxesSandboxIDLogs(c *gin.Context, sandboxID e2bapi.SandboxID, params e2bapi.GetSandboxesSandboxIDLogsParams) {
	limit := logLimit(params.Limit)
	entries, err := a.callbacks.GetSandboxLogs(a.callbackContext(c), sandboxID, SandboxLogsRequest{
		Start: params.Start,
		Limit: limit,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, e2bapi.SandboxLogs{
		Logs:       apiSandboxLogs(entries),
		LogEntries: apiSandboxLogEntries(entries),
	})
}

func (a *App) GetV2SandboxesSandboxIDLogs(c *gin.Context, sandboxID e2bapi.SandboxID, params e2bapi.GetV2SandboxesSandboxIDLogsParams) {
	limit := logLimit(params.Limit)
	entries, err := a.callbacks.GetSandboxLogsV2(a.callbackContext(c), sandboxID, SandboxLogsRequest{
		Cursor:    params.Cursor,
		Limit:     limit,
		Direction: params.Direction,
		Level:     params.Level,
		Search:    params.Search,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, e2bapi.SandboxLogsV2Response{
		Logs: apiSandboxLogEntries(entries),
	})
}

func (a *App) GetTemplates(c *gin.Context, params e2bapi.GetTemplatesParams) {
	templates, err := a.callbacks.ListTemplates(a.callbackContext(c), params)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, templates)
}

func (a *App) GetTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID, params e2bapi.GetTemplatesTemplateIDParams) {
	template, err := a.callbacks.GetTemplate(a.callbackContext(c), templateID, params)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, template)
}

func (a *App) GetTemplatesAliasesAlias(c *gin.Context, alias string) {
	template, err := a.callbacks.GetTemplateAlias(a.callbackContext(c), alias)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, template)
}

func (a *App) PostTemplates(c *gin.Context) {
	var req e2bapi.TemplateBuildRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	template, err := a.callbacks.CreateTemplate(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusAccepted, template)
}

func (a *App) PostV2Templates(c *gin.Context) {
	var req e2bapi.TemplateBuildRequestV2
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	template, err := a.callbacks.CreateTemplateV2(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusAccepted, template)
}

func (a *App) PostV3Templates(c *gin.Context) {
	var req e2bapi.TemplateBuildRequestV3
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	template, err := a.callbacks.CreateTemplateV3(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusAccepted, template)
}

func (a *App) DeleteTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	if err := a.callbacks.DeleteTemplate(a.callbackContext(c), templateID); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) PatchTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	var req e2bapi.TemplateUpdateRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.callbacks.UpdateTemplate(a.callbackContext(c), templateID, req); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusOK)
}

func (a *App) PatchV2TemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	var req e2bapi.TemplateUpdateRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	response, err := a.callbacks.UpdateTemplateV2(a.callbackContext(c), templateID, req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, response)
}

func (a *App) PostTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	var req e2bapi.TemplateBuildRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	template, err := a.callbacks.RebuildTemplate(a.callbackContext(c), templateID, req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusAccepted, template)
}

func (a *App) PostTemplatesTemplateIDBuildsBuildID(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID) {
	if err := a.callbacks.StartTemplateBuild(a.callbackContext(c), templateID, buildID); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusAccepted)
}

func (a *App) PostV2TemplatesTemplateIDBuildsBuildID(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID) {
	var req e2bapi.TemplateBuildStartV2
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.callbacks.StartTemplateBuildV2(a.callbackContext(c), templateID, buildID, req); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusAccepted)
}

func (a *App) GetTemplatesTemplateIDBuildsBuildIDStatus(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID, params e2bapi.GetTemplatesTemplateIDBuildsBuildIDStatusParams) {
	info, err := a.callbacks.GetTemplateBuildInfo(a.callbackContext(c), templateID, buildID, TemplateBuildInfoRequest{
		LogsOffset: params.LogsOffset,
		Limit:      params.Limit,
		Level:      params.Level,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, info)
}

func (a *App) GetTemplatesTemplateIDBuildsBuildIDLogs(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID, params e2bapi.GetTemplatesTemplateIDBuildsBuildIDLogsParams) {
	logs, err := a.callbacks.GetTemplateBuildLogs(a.callbackContext(c), templateID, buildID, TemplateBuildLogsRequest{
		Cursor:    params.Cursor,
		Limit:     params.Limit,
		Direction: params.Direction,
		Level:     params.Level,
		Source:    params.Source,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, logs)
}

func (a *App) GetTemplatesTemplateIDFilesHash(c *gin.Context, templateID e2bapi.TemplateID, hash string) {
	upload, err := a.callbacks.GetTemplateFileUpload(a.callbackContext(c), templateID, hash)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusCreated, upload)
}

func (a *App) PostTemplatesTags(c *gin.Context) {
	var req e2bapi.AssignTemplateTagsRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	tags, err := a.callbacks.AssignTemplateTags(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusCreated, tags)
}

func (a *App) DeleteTemplatesTags(c *gin.Context) {
	var req e2bapi.DeleteTemplateTagsRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if err := a.callbacks.DeleteTemplateTags(a.callbackContext(c), req); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) GetTemplatesTemplateIDTags(c *gin.Context, templateID e2bapi.TemplateID) {
	tags, err := a.callbacks.ListTemplateTags(a.callbackContext(c), templateID)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, tags)
}

func (a *App) GetTeams(c *gin.Context) {
	teams, err := a.callbacks.ListTeams(a.callbackContext(c))
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, teams)
}

func (a *App) GetTeamsTeamIDMetrics(c *gin.Context, teamID e2bapi.TeamID, params e2bapi.GetTeamsTeamIDMetricsParams) {
	metrics, err := a.callbacks.GetTeamMetrics(a.callbackContext(c), teamID, TeamMetricsRequest{
		Start: params.Start,
		End:   params.End,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, metrics)
}

func (a *App) GetTeamsTeamIDMetricsMax(c *gin.Context, teamID e2bapi.TeamID, params e2bapi.GetTeamsTeamIDMetricsMaxParams) {
	metric, err := a.callbacks.GetTeamMetricMax(a.callbackContext(c), teamID, TeamMetricMaxRequest{
		Start:  params.Start,
		End:    params.End,
		Metric: params.Metric,
	})
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, metric)
}

func (a *App) PostAdminTeamsTeamIDSandboxesKill(c *gin.Context, teamID openapi_types.UUID) {
	result, err := a.callbacks.KillTeamSandboxes(a.callbackContext(c), teamID.String())
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (a *App) PostAdminTeamsTeamIDBuildsCancel(c *gin.Context, teamID openapi_types.UUID) {
	result, err := a.callbacks.CancelTeamBuilds(a.callbackContext(c), teamID.String())
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, result)
}

func (a *App) GetVolumes(c *gin.Context) {
	volumes, err := a.callbacks.ListVolumes(a.callbackContext(c))
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, volumes)
}

func (a *App) PostVolumes(c *gin.Context) {
	var req e2bapi.NewVolume
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	volume, err := a.callbacks.CreateVolume(a.callbackContext(c), req)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusCreated, volume)
}

func (a *App) GetVolumesVolumeID(c *gin.Context, volumeID e2bapi.VolumeID) {
	volume, err := a.callbacks.GetVolume(a.callbackContext(c), volumeID)
	if err != nil {
		writeGatewayError(c, err, http.StatusNotFound)
		return
	}

	c.JSON(http.StatusOK, volume)
}

func (a *App) DeleteVolumesVolumeID(c *gin.Context, volumeID e2bapi.VolumeID) {
	if err := a.callbacks.DeleteVolume(a.callbackContext(c), volumeID); err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) handleUnimplementedGatewayRoute(c *gin.Context) {
	writeNotImplemented(c)
}

func (a *App) callbackContext(c *gin.Context) context.Context {
	return contextWithInboundRequest(contextWithInboundHeaders(c.Request.Context(), c.Request.Header), c.Request)
}

func (a *App) sandboxDomainForResponse(record SandboxRecord) *string {
	return nil
}

func (a *App) apiSandboxResponse(record SandboxRecord) sandboxAPIResponse {
	return sandboxAPIResponse{
		Sandbox: e2bapi.Sandbox{
			Alias:       optionalString(record.Alias),
			ClientID:    sandboxClientID(record),
			Domain:      a.sandboxDomainForResponse(record),
			EnvdVersion: sandboxEnvdVersion(record),
			SandboxID:   record.ID,
			TemplateID:  record.TemplateID,
		},
		EnvdURL:      record.EnvdURL,
		VolumeMounts: record.RuntimeInfo.VolumeMounts,
	}
}

func (a *App) apiSandboxDetailResponse(record SandboxRecord) e2bapi.SandboxDetail {
	metadata := apiSandboxMetadata(record.Metadata)
	volumeMounts := apiSandboxVolumeMounts(record.RuntimeInfo.VolumeMounts)
	endAt := record.EndAt
	if endAt.IsZero() {
		endAt = defaultSandboxEndAt(record.CreatedAt)
	}

	return e2bapi.SandboxDetail{
		Alias:               optionalString(record.Alias),
		AllowInternetAccess: record.InternetAccessPolicy.BoolPtr(),
		ClientID:            sandboxClientID(record),
		CpuCount:            sandboxCPUCount(record),
		DiskSizeMB:          sandboxDiskSizeMB(record),
		Domain:              a.sandboxDomainForResponse(record),
		EndAt:               endAt,
		EnvdVersion:         sandboxEnvdVersion(record),
		Lifecycle: &e2bapi.SandboxLifecycle{
			AutoResume: false,
			OnTimeout:  e2bapi.Kill,
		},
		MemoryMB:     sandboxMemoryMB(record),
		Metadata:     metadata,
		SandboxID:    record.ID,
		StartedAt:    record.CreatedAt,
		State:        apiSandboxState(record.State),
		TemplateID:   record.TemplateID,
		VolumeMounts: volumeMounts,
	}
}

func (a *App) apiListedSandboxes(records []SandboxRecord) []listedSandboxAPIResponse {
	items := make([]listedSandboxAPIResponse, 0, len(records))
	for _, record := range records {
		items = append(items, listedSandboxAPIResponse{
			ListedSandbox: e2bapi.ListedSandbox{
				Alias:        optionalString(record.Alias),
				ClientID:     sandboxClientID(record),
				CpuCount:     sandboxCPUCount(record),
				DiskSizeMB:   sandboxDiskSizeMB(record),
				EndAt:        recordEndAt(record),
				EnvdVersion:  sandboxEnvdVersion(record),
				MemoryMB:     sandboxMemoryMB(record),
				Metadata:     apiSandboxMetadata(record.Metadata),
				SandboxID:    record.ID,
				StartedAt:    record.CreatedAt,
				State:        apiSandboxState(record.State),
				TemplateID:   record.TemplateID,
				VolumeMounts: apiSandboxVolumeMounts(record.RuntimeInfo.VolumeMounts),
			},
			EnvdURL: record.EnvdURL,
		})
	}
	return items
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (a *App) gatewayTemplate(template SandboxRuntimeTemplate) GatewayTemplate {
	names := template.Names
	if len(names) == 0 && template.TemplateID != "" {
		names = []string{template.TemplateID}
	}

	createdAt := template.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := template.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	return GatewayTemplate{
		Template: e2bapi.Template{
			Aliases:       []string{},
			BuildCount:    int32(template.BuildCount),
			BuildID:       template.BuildID,
			BuildStatus:   e2bapi.TemplateBuildStatus(template.BuildStatus),
			CpuCount:      int32(template.CPUCount),
			CreatedAt:     createdAt,
			CreatedBy:     nil,
			DiskSizeMB:    int32(template.DiskSizeMB),
			EnvdVersion:   defaultEnvdVersion,
			LastSpawnedAt: template.LastSpawnedAt,
			MemoryMB:      int32(template.MemoryMB),
			Names:         names,
			Public:        template.Public,
			SpawnCount:    template.SpawnCount,
			TemplateID:    template.TemplateID,
			UpdatedAt:     updatedAt,
		},
		ImageRef: template.ImageRef,
	}
}

func apiVolume(volume RuntimeVolume) e2bapi.Volume {
	return e2bapi.Volume{
		VolumeID: volume.VolumeID,
		Name:     volume.Name,
	}
}

func apiVolumeAndToken(volume RuntimeVolume) e2bapi.VolumeAndToken {
	return e2bapi.VolumeAndToken{
		VolumeID: volume.VolumeID,
		Name:     volume.Name,
		Token:    apiCompatibleVolumeToken(volume),
	}
}

func apiCompatibleVolumeToken(volume RuntimeVolume) string {
	if volumeID := strings.TrimSpace(volume.VolumeID); volumeID != "" {
		return "compat-volume-token-" + volumeID
	}
	return ""
}

func apiSandboxState(state string) e2bapi.SandboxState {
	switch strings.ToLower(state) {
	case string(e2bapi.Paused):
		return e2bapi.Paused
	default:
		return e2bapi.Running
	}
}

func APISandboxState(state string) e2bapi.SandboxState {
	return apiSandboxState(state)
}

func apiSandboxMetadata(metadata map[string]string) *e2bapi.SandboxMetadata {
	if len(metadata) == 0 {
		return nil
	}
	copied := e2bapi.SandboxMetadata(copyStringMap(metadata))
	return &copied
}

func apiSandboxVolumeMounts(volumeMounts []VolumeMount) *[]e2bapi.SandboxVolumeMount {
	normalized := normalizeVolumeMounts(volumeMounts)
	if len(normalized) == 0 {
		return nil
	}

	result := make([]e2bapi.SandboxVolumeMount, 0, len(normalized))
	for _, volumeMount := range normalized {
		result = append(result, e2bapi.SandboxVolumeMount{
			Name: volumeMount.Name,
			Path: volumeMount.Path,
		})
	}
	return &result
}

func recordEndAt(record SandboxRecord) time.Time {
	if !record.EndAt.IsZero() {
		return record.EndAt
	}
	return defaultSandboxEndAt(record.CreatedAt)
}

func sandboxClientID(record SandboxRecord) string {
	if record.ClientID != "" {
		return record.ClientID
	}
	return "e2b-local"
}

func sandboxEnvdVersion(record SandboxRecord) e2bapi.EnvdVersion {
	if record.EnvdVersion != "" {
		return record.EnvdVersion
	}
	return defaultEnvdVersion
}

func sandboxCPUCount(record SandboxRecord) e2bapi.CPUCount {
	if record.CPUCount > 0 {
		return record.CPUCount
	}
	return 1
}

func sandboxDiskSizeMB(record SandboxRecord) e2bapi.DiskSizeMB {
	if record.DiskSizeMB > 0 {
		return record.DiskSizeMB
	}
	return 0
}

func sandboxMemoryMB(record SandboxRecord) e2bapi.MemoryMB {
	if record.MemoryMB > 0 {
		return record.MemoryMB
	}
	return 512
}

func SandboxMemoryMB(record SandboxRecord) e2bapi.MemoryMB {
	return sandboxMemoryMB(record)
}

func bindOptionalJSON(c *gin.Context, v any) error {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}

	err := bindJSON(c, v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func writeGatewayError(c *gin.Context, err error, fallback int) {
	writeError(c, gatewayErrorStatus(err, fallback), err.Error())
}

func logLimit(limit *int32) int32 {
	if limit == nil || *limit <= 0 {
		return 1000
	}
	if *limit > 1000 {
		return 1000
	}
	return *limit
}

func apiSandboxLogEntries(entries []SandboxRuntimeLogEntry) []e2bapi.SandboxLogEntry {
	result := make([]e2bapi.SandboxLogEntry, 0, len(entries))
	for _, entry := range entries {
		level := entry.Level
		if level == "" {
			level = e2bapi.LogLevelInfo
		}
		timestamp := entry.Timestamp
		if timestamp.IsZero() {
			timestamp = time.Now().UTC()
		}
		result = append(result, e2bapi.SandboxLogEntry{
			Fields:    copyStringMap(entry.Fields),
			Level:     level,
			Message:   entry.Message,
			Timestamp: timestamp,
		})
	}
	return result
}

func apiSandboxLogs(entries []SandboxRuntimeLogEntry) []e2bapi.SandboxLog {
	result := make([]e2bapi.SandboxLog, 0, len(entries))
	for _, entry := range entries {
		timestamp := entry.Timestamp
		if timestamp.IsZero() {
			timestamp = time.Now().UTC()
		}
		result = append(result, e2bapi.SandboxLog{
			Line:      entry.Message,
			Timestamp: timestamp,
		})
	}
	return result
}
