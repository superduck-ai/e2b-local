package gateway

import (
	"net/http"

	"e2b-local/internal/e2bapi"

	"github.com/gin-gonic/gin"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

type unimplementedGatewayAPI struct {
	fallback func(c *gin.Context)
}

func (api unimplementedGatewayAPI) writeNotImplemented(c *gin.Context) {
	if api.fallback != nil {
		api.fallback(c)
		return
	}
	writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostAccessTokens(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) DeleteAccessTokensAccessTokenID(c *gin.Context, accessTokenID e2bapi.AccessTokenID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostAdminTeamsTeamIDBuildsCancel(c *gin.Context, teamID openapi_types.UUID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostAdminTeamsTeamIDSandboxesKill(c *gin.Context, teamID openapi_types.UUID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetApiKeys(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostApiKeys(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) DeleteApiKeysApiKeyID(c *gin.Context, apiKeyID e2bapi.ApiKeyID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PatchApiKeysApiKeyID(c *gin.Context, apiKeyID e2bapi.ApiKeyID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetHealth(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetNodes(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetNodesNodeID(c *gin.Context, nodeID e2bapi.NodeID, params e2bapi.GetNodesNodeIDParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostNodesNodeID(c *gin.Context, nodeID e2bapi.NodeID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetSandboxes(c *gin.Context, params e2bapi.GetSandboxesParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxes(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetSandboxesMetrics(c *gin.Context, params e2bapi.GetSandboxesMetricsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) DeleteSandboxesSandboxID(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetSandboxesSandboxID(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxesSandboxIDConnect(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetSandboxesSandboxIDLogs(c *gin.Context, sandboxID e2bapi.SandboxID, params e2bapi.GetSandboxesSandboxIDLogsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetSandboxesSandboxIDMetrics(c *gin.Context, sandboxID e2bapi.SandboxID, params e2bapi.GetSandboxesSandboxIDMetricsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PutSandboxesSandboxIDNetwork(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxesSandboxIDPause(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxesSandboxIDRefreshes(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxesSandboxIDResume(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxesSandboxIDSnapshots(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostSandboxesSandboxIDTimeout(c *gin.Context, sandboxID e2bapi.SandboxID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetSnapshots(c *gin.Context, params e2bapi.GetSnapshotsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTeams(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTeamsTeamIDMetrics(c *gin.Context, teamID e2bapi.TeamID, params e2bapi.GetTeamsTeamIDMetricsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTeamsTeamIDMetricsMax(c *gin.Context, teamID e2bapi.TeamID, params e2bapi.GetTeamsTeamIDMetricsMaxParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplates(c *gin.Context, params e2bapi.GetTemplatesParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostTemplates(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplatesAliasesAlias(c *gin.Context, alias string) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) DeleteTemplatesTags(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostTemplatesTags(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) DeleteTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID, params e2bapi.GetTemplatesTemplateIDParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PatchTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostTemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostTemplatesTemplateIDBuildsBuildID(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplatesTemplateIDBuildsBuildIDLogs(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID, params e2bapi.GetTemplatesTemplateIDBuildsBuildIDLogsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplatesTemplateIDBuildsBuildIDStatus(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID, params e2bapi.GetTemplatesTemplateIDBuildsBuildIDStatusParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplatesTemplateIDFilesHash(c *gin.Context, templateID e2bapi.TemplateID, hash string) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetTemplatesTemplateIDTags(c *gin.Context, templateID e2bapi.TemplateID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetV2Sandboxes(c *gin.Context, params e2bapi.GetV2SandboxesParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetV2SandboxesSandboxIDLogs(c *gin.Context, sandboxID e2bapi.SandboxID, params e2bapi.GetV2SandboxesSandboxIDLogsParams) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostV2Templates(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PatchV2TemplatesTemplateID(c *gin.Context, templateID e2bapi.TemplateID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostV2TemplatesTemplateIDBuildsBuildID(c *gin.Context, templateID e2bapi.TemplateID, buildID e2bapi.BuildID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostV3Templates(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetVolumes(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) PostVolumes(c *gin.Context) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) DeleteVolumesVolumeID(c *gin.Context, volumeID e2bapi.VolumeID) {
	api.writeNotImplemented(c)
}

func (api unimplementedGatewayAPI) GetVolumesVolumeID(c *gin.Context, volumeID e2bapi.VolumeID) {
	api.writeNotImplemented(c)
}

func writeNotImplemented(c *gin.Context) {
	writeError(c, http.StatusNotImplemented, "route is registered but not implemented by e2b-local")
}
