package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"

	"github.com/docker/docker/errdefs"
	"github.com/gin-gonic/gin"
)

const maxRequestBodyBytes = 1 << 20
const maxTemplateFileUploadBytes = 512 << 20
const runtimeRestoreTimeout = 10 * time.Second

type App struct {
	unimplementedGatewayAPI

	cfg        Config
	logger     *log.Logger
	store      *SandboxStore
	management *GatewayManagementStore
	runtime    SandboxRuntime
	callbacks  GatewayCallbacks
}

func NewApp(cfg Config, logger *log.Logger) http.Handler {
	handler, err := NewAppWithRuntime(cfg, logger, nil)
	if err != nil {
		panic(fmt.Sprintf("create app: %v", err))
	}
	return handler
}

func NewAppWithRuntime(cfg Config, logger *log.Logger, runtime SandboxRuntime) (http.Handler, error) {
	return NewAppWithCallbacks(cfg, logger, runtime, GatewayCallbacks{})
}

func NewAppWithCallbacks(cfg Config, logger *log.Logger, runtime SandboxRuntime, callbacks GatewayCallbacks) (http.Handler, error) {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	if runtime == nil {
		var err error
		runtime, err = NewSandboxRuntime(cfg, logger)
		if err != nil {
			return nil, err
		}
	}

	management := NewGatewayManagementStore()
	store := NewSandboxStore()

	app := &App{
		cfg:        cfg,
		logger:     logger,
		store:      store,
		management: management,
		runtime:    runtime,
	}
	app.callbacks = callbacks.WithDefaults(DefaultGatewayCallbacks(app))
	app.unimplementedGatewayAPI.fallback = app.handleUnimplementedGatewayRoute
	if err := app.restoreRuntimeSandboxes(context.Background()); err != nil {
		return nil, err
	}
	if err := app.reconcileStoredSandboxes(context.Background()); err != nil {
		return nil, err
	}

	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(app.corsMiddleware())
	router.Use(app.loggingMiddleware())

	e2bapi.RegisterHandlersWithOptions(router, app, e2bapi.GinServerOptions{
		ErrorHandler: func(c *gin.Context, err error, statusCode int) {
			writeError(c, statusCode, err.Error())
		},
	})

	router.GET("/healthz", app.handleHealth)
	router.GET("/v2/templates", func(c *gin.Context) {
		teamIDValue := c.Query("teamID")
		var teamID *string
		if teamIDValue != "" {
			teamID = &teamIDValue
		}
		app.GetTemplates(c, e2bapi.GetTemplatesParams{TeamID: teamID})
	})
	router.PUT("/_e2b/template-files/:templateID/:hash", app.handleTemplateFileUpload)
	router.NoRoute(app.handleNoRoute)

	return router, nil
}

func (a *App) restoreRuntimeSandboxes(ctx context.Context) error {
	restorer, ok := a.runtime.(SandboxRuntimeRestorer)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, runtimeRestoreTimeout)
	defer cancel()

	records, err := restorer.RestoreSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("restore runtime sandboxes: %w", err)
	}

	restored := 0
	for _, record := range records {
		record, err = a.enrichRestoredSandboxRecord(record)
		if err != nil {
			return err
		}
		created := record
		if existing, exists := a.store.Get(record.ID); exists {
			runtimeInfo := mergeSandboxRuntimeInfo(existing.RuntimeInfo, record.RuntimeInfo)
			updated, ok, err := a.store.SetStateRuntimeInfoAndEndAt(record.ID, record.State, runtimeInfo, record.EndAt)
			if err != nil {
				return fmt.Errorf("restore sandbox %s: %w", record.ID, err)
			}
			if ok {
				created = updated
			}
		} else {
			created, err = a.store.Create(record)
			if err != nil {
				return fmt.Errorf("restore sandbox %s: %w", record.ID, err)
			}
		}
		if _, exists, err := a.reconcileSandboxRecord(ctx, created); err != nil {
			return err
		} else if exists {
			restored++
		}
	}
	if restored > 0 {
		a.logger.Printf("sandbox restore count=%d", restored)
	}
	return nil
}

func (a *App) reconcileStoredSandboxes(ctx context.Context) error {
	if len(a.store.List()) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, runtimeRestoreTimeout)
	defer cancel()

	records, err := a.reconcileSandboxRecords(ctx, a.store.List())
	if err != nil {
		return fmt.Errorf("reconcile sandbox state: %w", err)
	}
	if len(records) > 0 {
		a.logger.Printf("sandbox persistent state count=%d", len(records))
	}
	return nil
}

func (a *App) enrichRestoredSandboxRecord(record SandboxRecord) (SandboxRecord, error) {
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.EndAt.IsZero() {
		record.EndAt = time.Now().UTC().Add(time.Duration(defaultSandboxTimeoutSeconds) * time.Second)
	}
	if strings.TrimSpace(record.State) == "" {
		record.State = string(e2bapi.Running)
	}
	return record, nil
}

func (a *App) handleNoRoute(c *gin.Context) {
	writeError(c, http.StatusNotFound, "route not found")
}

func (a *App) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
	})
}

func (a *App) handleTemplateFileUpload(c *gin.Context) {
	templateID := strings.TrimSpace(c.Param("templateID"))
	hash := strings.TrimSpace(c.Param("hash"))
	token := strings.TrimSpace(c.Query("token"))
	if templateID == "" || hash == "" || token == "" {
		writeError(c, http.StatusBadRequest, "templateID, hash, and token are required")
		return
	}

	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxTemplateFileUploadBytes)
	data, err := io.ReadAll(reader)
	if err != nil {
		writeError(c, http.StatusRequestEntityTooLarge, "template file upload is too large")
		return
	}
	if len(data) == 0 {
		writeError(c, http.StatusBadRequest, "template file upload body is required")
		return
	}

	stored, err := a.management.StoreTemplateFileUpload(templateID, hash, token, data)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}
	if !stored {
		writeError(c, http.StatusUnauthorized, "invalid or expired template file upload token")
		return
	}

	c.Status(http.StatusOK)
}

func (a *App) handleCreateSandbox(c *gin.Context) {
	var req NewSandboxRequest
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	req.TemplateID = strings.TrimSpace(req.TemplateID)
	if req.TemplateID == "" {
		writeError(c, http.StatusBadRequest, "templateID is required")
		return
	}

	if err := a.validateTemplateID(req.TemplateID); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	sandboxID, err := newSandboxID()
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	runtimeInfo, err := a.runtime.CreateSandbox(c.Request.Context(), SandboxRuntimeCreateRequest{
		SandboxID:    sandboxID,
		TemplateID:   req.TemplateID,
		Metadata:     req.Metadata,
		EnvVars:      req.EnvVars,
		VolumeMounts: req.VolumeMounts,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	record, err := a.store.Create(SandboxRecord{
		ID:          sandboxID,
		TemplateID:  req.TemplateID,
		Metadata:    req.Metadata,
		EnvdURL:     runtimeInfo.EnvdURL,
		RuntimeInfo: runtimeInfo,
	})
	if err != nil {
		if cleanupErr := a.runtime.DeleteSandbox(c.Request.Context(), runtimeInfo); cleanupErr != nil {
			a.logger.Printf("sandbox create cleanup failed sandbox_id=%s error=%v", sandboxID, cleanupErr)
		}
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	a.logger.Printf("sandbox create sandbox_id=%s template_id=%s envd_url=%s container_id=%s",
		record.ID,
		record.TemplateID,
		record.EnvdURL,
		record.RuntimeInfo.ContainerID,
	)

	c.JSON(http.StatusCreated, a.sandboxResponse(record))
}

func (a *App) validateTemplateID(templateID string) error {
	if a.cfg.Runtime.Type != "docker" {
		return nil
	}

	return nil
}

func (a *App) handleListTemplates(c *gin.Context) {
	templates, err := a.runtime.ListTemplates(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	response := make([]TemplateResponse, 0, len(templates))
	for _, template := range templates {
		response = append(response, a.templateResponse(template))
	}

	c.JSON(http.StatusOK, response)
}

func (a *App) handleListVolumes(c *gin.Context) {
	volumeRuntime, ok := a.volumeRuntime(c)
	if !ok {
		return
	}

	volumes, err := volumeRuntime.ListVolumes(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	response := make([]VolumeResponse, 0, len(volumes))
	for _, volume := range volumes {
		response = append(response, a.volumeResponse(volume))
	}

	c.JSON(http.StatusOK, response)
}

func (a *App) handleCreateVolume(c *gin.Context) {
	volumeRuntime, ok := a.volumeRuntime(c)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := bindJSON(c, &req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(c, http.StatusBadRequest, "name is required")
		return
	}

	volume, err := volumeRuntime.CreateVolume(c.Request.Context(), name)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusCreated, a.volumeResponse(volume))
}

func (a *App) handleGetVolume(c *gin.Context) {
	volumeRuntime, ok := a.volumeRuntime(c)
	if !ok {
		return
	}

	volumeID := c.Param("volumeID")
	if volumeID == "" {
		writeError(c, http.StatusNotFound, "volume not found")
		return
	}

	volume, err := volumeRuntime.GetVolume(c.Request.Context(), volumeID)
	if err != nil {
		writeError(c, volumeErrorStatus(err), err.Error())
		return
	}

	c.JSON(http.StatusOK, a.volumeResponse(volume))
}

func (a *App) handleDeleteVolume(c *gin.Context) {
	volumeRuntime, ok := a.volumeRuntime(c)
	if !ok {
		return
	}

	volumeID := c.Param("volumeID")
	if volumeID == "" {
		writeError(c, http.StatusNotFound, "volume not found")
		return
	}

	deleted, err := volumeRuntime.DeleteVolume(c.Request.Context(), volumeID)
	if err != nil {
		writeError(c, volumeErrorStatus(err), err.Error())
		return
	}
	if !deleted {
		writeError(c, http.StatusNotFound, fmt.Sprintf("volume %s not found", volumeID))
		return
	}

	c.Status(http.StatusNoContent)
}

func (a *App) handleListSandboxes(c *gin.Context) {
	records := a.store.List()
	items := make([]ListedSandboxResponse, 0, len(records))
	for _, record := range records {
		items = append(items, a.listedSandboxResponse(record))
	}

	c.JSON(http.StatusOK, items)
}

func (a *App) handleConnectSandbox(c *gin.Context) {
	id := c.Param("sandboxID")
	if id == "" {
		writeError(c, http.StatusNotFound, "sandbox not found")
		return
	}

	record, ok := a.store.Get(id)
	if !ok {
		writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", id))
		return
	}

	if record.State == "paused" {
		runtimeInfo, err := a.runtime.ResumeSandbox(c.Request.Context(), record.RuntimeInfo)
		if err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}

		var updated bool
		record, updated, err = a.store.SetStateAndRuntimeInfo(id, "running", runtimeInfo)
		if err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
		if !updated {
			writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", id))
			return
		}
	}

	a.logger.Printf("sandbox connect sandbox_id=%s envd_url=%s", record.ID, record.EnvdURL)
	c.JSON(http.StatusOK, a.sandboxResponse(record))
}

func (a *App) handlePauseSandbox(c *gin.Context) {
	id := c.Param("sandboxID")
	if id == "" {
		writeError(c, http.StatusNotFound, "sandbox not found")
		return
	}

	record, ok := a.store.Get(id)
	if !ok {
		writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", id))
		return
	}

	if record.State == "paused" {
		writeError(c, http.StatusConflict, "already paused")
		return
	}

	if err := a.runtime.PauseSandbox(c.Request.Context(), record.RuntimeInfo); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	record, ok, err := a.store.SetState(id, "paused")
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", id))
		return
	}

	a.logger.Printf("sandbox pause sandbox_id=%s action=mark_paused", record.ID)
	c.JSON(http.StatusOK, gin.H{"paused": true})
}

func (a *App) handleDeleteSandbox(c *gin.Context) {
	id := c.Param("sandboxID")
	if id == "" {
		writeError(c, http.StatusNotFound, "sandbox not found")
		return
	}

	record, ok := a.store.Get(id)
	if !ok {
		writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", id))
		return
	}

	if err := a.runtime.DeleteSandbox(c.Request.Context(), record.RuntimeInfo); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	deleted, err := a.store.Delete(id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", id))
		return
	}

	a.logger.Printf("sandbox delete sandbox_id=%s action=delete_mapping container_id=%s", id, record.RuntimeInfo.ContainerID)
	c.Status(http.StatusNoContent)
}

func (a *App) sandboxResponse(record SandboxRecord) SandboxResponse {
	return SandboxResponse{
		ClientID:     "e2b-local",
		Domain:       nil,
		EnvdVersion:  defaultEnvdVersion,
		EnvdURL:      record.EnvdURL,
		SandboxID:    record.ID,
		TemplateID:   record.TemplateID,
		VolumeMounts: record.RuntimeInfo.VolumeMounts,
	}
}

func (a *App) volumeResponse(volume RuntimeVolume) VolumeResponse {
	return VolumeResponse{
		VolumeID: volume.VolumeID,
		Name:     volume.Name,
		Token:    apiCompatibleVolumeToken(volume),
	}
}

func (a *App) templateResponse(template SandboxRuntimeTemplate) TemplateResponse {
	names := template.Names
	if len(names) == 0 && template.TemplateID != "" {
		names = []string{template.TemplateID}
	}

	return TemplateResponse{
		TemplateID:    template.TemplateID,
		Aliases:       []string{},
		Names:         names,
		ImageRef:      template.ImageRef,
		BuildCount:    template.BuildCount,
		BuildID:       template.BuildID,
		BuildStatus:   template.BuildStatus,
		CPUCount:      template.CPUCount,
		DiskSizeMB:    template.DiskSizeMB,
		EnvdVersion:   defaultEnvdVersion,
		LastSpawnedAt: template.LastSpawnedAt,
		MemoryMB:      template.MemoryMB,
		Public:        template.Public,
		SpawnCount:    template.SpawnCount,
		CreatedAt:     template.CreatedAt,
		UpdatedAt:     template.UpdatedAt,
	}
}

func (a *App) listedSandboxResponse(record SandboxRecord) ListedSandboxResponse {
	return ListedSandboxResponse{
		ClientID:     "e2b-local",
		CPUCount:     1,
		DiskSizeMB:   0,
		EndAt:        record.CreatedAt.Add(5 * time.Minute),
		EnvdVersion:  defaultEnvdVersion,
		MemoryMB:     512,
		Metadata:     record.Metadata,
		SandboxID:    record.ID,
		StartedAt:    record.CreatedAt,
		State:        record.State,
		TemplateID:   record.TemplateID,
		VolumeMounts: record.RuntimeInfo.VolumeMounts,
	}
}

func (a *App) volumeRuntime(c *gin.Context) (VolumeRuntime, bool) {
	volumeRuntime, ok := a.runtime.(VolumeRuntime)
	if !ok {
		writeError(c, http.StatusNotImplemented, "volumes are not supported by this runtime")
		return nil, false
	}
	return volumeRuntime, true
}

func volumeErrorStatus(err error) int {
	if errdefs.IsNotFound(err) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func bindJSON(c *gin.Context, v any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	return c.ShouldBindJSON(v)
}

func writeError(c *gin.Context, status int, message string) {
	c.JSON(status, ErrorResponse{
		Code:    status,
		Message: message,
	})
}

func (a *App) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		a.logger.Printf("request method=%s path=%s status=%d duration_ms=%d", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start).Milliseconds())
	}
}

func (a *App) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Vary", "Origin")

		if c.Request.Method == http.MethodOptions {
			c.Status(http.StatusNoContent)
			c.Abort()
			return
		}

		c.Next()
	}
}
