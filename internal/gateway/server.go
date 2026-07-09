package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"e2b-local/internal/e2bapi"

	"github.com/docker/docker/errdefs"
	"github.com/gin-gonic/gin"
)

const maxRequestBodyBytes = 1 << 20
const runtimeRestoreTimeout = 10 * time.Second

type App struct {
	unimplementedGatewayAPI

	cfg        Config
	logger     *log.Logger
	router     http.Handler
	store      *SandboxStore
	management *GatewayManagementStore
	builds     *templateBuildManager
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
	builds := newTemplateBuildManager(cfg.TemplateBuilds.MaxConcurrent)

	app := &App{
		cfg:        cfg,
		logger:     logger,
		store:      store,
		management: management,
		builds:     builds,
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
	router.GET("/sandboxes/:sandboxID/ports/:port", app.handleGetSandboxPort)
	router.GET("/volumecontent/:volumeID/path", app.handleVolumeContentPathGet)
	router.GET("/volumecontent/:volumeID/file", app.handleVolumeContentFileGet)
	router.PUT("/volumecontent/:volumeID/file", app.handleVolumeContentFilePut)
	router.GET("/volumecontent/:volumeID/dir", app.handleVolumeContentDirGet)
	router.POST("/volumecontent/:volumeID/dir", app.handleVolumeContentDirPost)
	router.PUT("/_e2b/template-files/:templateID/:hash", app.handleTemplateFileUpload)
	router.NoRoute(app.handleNoRoute)

	app.router = router
	return app, nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.router.ServeHTTP(w, r)
}

func (a *App) Shutdown(ctx context.Context) error {
	if a.builds == nil {
		return nil
	}
	return a.builds.shutdown(ctx)
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
	stored, err := a.management.StoreTemplateFileUploadReader(templateID, hash, token, reader)
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

func (a *App) validateTemplateID(templateID string) error {
	if a.cfg.Runtime.Type != "docker" {
		return nil
	}

	return nil
}

func (a *App) handleGetSandboxPort(c *gin.Context) {
	sandboxID := strings.TrimSpace(c.Param("sandboxID"))
	if sandboxID == "" {
		writeError(c, http.StatusNotFound, "sandbox not found")
		return
	}

	containerPort, err := strconv.Atoi(strings.TrimSpace(c.Param("port")))
	if err != nil || containerPort <= 0 || containerPort > 65535 {
		writeError(c, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}

	record, err := a.callbacks.GetSandbox(a.callbackContext(c), sandboxID)
	if err != nil {
		writeGatewayError(c, err, http.StatusNotFound)
		return
	}

	mapping, ok := sandboxPortMapping(record.RuntimeInfo.PublishedPorts, containerPort)
	if !ok {
		writeError(c, http.StatusNotFound, fmt.Sprintf("sandbox %s port %d is not published; configure docker.published_ports or Dockerfile EXPOSE", sandboxID, containerPort))
		return
	}

	host, err := a.advertisedTrafficHost()
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	c.JSON(http.StatusOK, sandboxPortResponse(mapping, host))
}

func (a *App) advertisedTrafficHost() (string, error) {
	host := strings.TrimSpace(a.cfg.Traffic.AdvertisedHost)
	if host != "" {
		return host, nil
	}
	return DetectTrafficAdvertisedHost(a.cfg.Traffic)
}

func sandboxPortMapping(mappings []SandboxPortMapping, containerPort int) (SandboxPortMapping, bool) {
	for _, mapping := range mappings {
		if mapping.ContainerPort != containerPort {
			continue
		}
		if mapping.HostPort <= 0 {
			continue
		}
		if protocol := strings.ToLower(strings.TrimSpace(mapping.Protocol)); protocol != "" && protocol != "tcp" {
			continue
		}
		if strings.TrimSpace(mapping.Protocol) == "" {
			mapping.Protocol = "tcp"
		}
		return mapping, true
	}
	return SandboxPortMapping{}, false
}

func sandboxPortResponse(mapping SandboxPortMapping, host string) SandboxPortResponse {
	host = strings.TrimSpace(host)
	hostPort := net.JoinHostPort(host, strconv.Itoa(mapping.HostPort))
	protocol := strings.ToLower(strings.TrimSpace(mapping.Protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	return SandboxPortResponse{
		ContainerPort: mapping.ContainerPort,
		Host:          host,
		HostPort:      mapping.HostPort,
		URL:           "http://" + hostPort,
		WSURL:         "ws://" + hostPort,
		Protocol:      protocol,
	}
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
