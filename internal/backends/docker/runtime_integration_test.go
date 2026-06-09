//go:build docker_runtime_integration

package dockerbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"

	"github.com/docker/docker/errdefs"
)

func dockerIntegrationTemplate(t *testing.T, runtime *DockerRuntime, ctx context.Context) SandboxRuntimeTemplate {
	t.Helper()

	templates, err := runtime.ListTemplates(ctx)
	if err != nil {
		t.Skipf("list docker templates: %v", err)
	}
	if len(templates) == 0 {
		t.Skip("no tagged docker images are available as templates")
	}
	return templates[0]
}

func skipUnlessDockerIntegrationEnvdAvailable(t *testing.T, runtime *DockerRuntime, ctx context.Context, imageRef string) {
	t.Helper()

	inspect, _, err := runtime.client.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		t.Skipf("docker image is unavailable: %v", err)
	}
	envdBinary, err := runtime.envdBinaryForPlatform(dockerImagePlatform(inspect))
	if err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}
	if _, err := os.Stat(envdBinary); err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}
}

func TestDockerRuntimeCreatePauseResumeDelete(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runtime, err := NewDockerRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("docker runtime is unavailable: %v", err)
	}

	template := dockerIntegrationTemplate(t, runtime, ctx)
	skipUnlessDockerIntegrationEnvdAvailable(t, runtime, ctx, template.ImageRef)

	_ = runtime.DeleteSandbox(context.Background(), SandboxRuntimeInfo{
		ContainerID: cfg.ContainerNamePrefix + "sbx_integration",
	})

	info, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  "sbx_integration",
		TemplateID: template.TemplateID,
	})
	if err != nil {
		t.Fatalf("create docker sandbox: %v", err)
	}
	defer func() {
		if err := runtime.DeleteSandbox(context.Background(), info); err != nil {
			t.Fatalf("delete docker sandbox: %v", err)
		}
	}()

	if info.ContainerID == "" {
		t.Fatal("expected container id")
	}

	if info.EnvdURL == "" {
		t.Fatal("expected envd url")
	}

	if err := runtime.PauseSandbox(ctx, info); err != nil {
		t.Fatalf("pause docker sandbox: %v", err)
	}

	if _, err := runtime.ResumeSandbox(ctx, info); err != nil {
		t.Fatalf("resume docker sandbox: %v", err)
	}

	resp, err := http.Get(info.EnvdURL + "/health")
	if err != nil {
		t.Fatalf("check resumed envd health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("expected healthy envd status, got %d", resp.StatusCode)
	}
}

func TestDockerRuntimeDynamicPortsAllowMultipleSandboxes(t *testing.T) {
	cfg := gateway.DefaultConfig().Docker

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	runtime, err := NewDockerRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("docker runtime is unavailable: %v", err)
	}

	template := dockerIntegrationTemplate(t, runtime, ctx)
	skipUnlessDockerIntegrationEnvdAvailable(t, runtime, ctx, template.ImageRef)

	first, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  "sbx_dynamic_ports_1",
		TemplateID: template.TemplateID,
	})
	if err != nil {
		t.Fatalf("create first docker sandbox: %v", err)
	}
	defer func() {
		if err := runtime.DeleteSandbox(context.Background(), first); err != nil {
			t.Fatalf("delete first docker sandbox: %v", err)
		}
	}()

	second, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  "sbx_dynamic_ports_2",
		TemplateID: template.TemplateID,
	})
	if err != nil {
		t.Fatalf("create second docker sandbox: %v", err)
	}
	defer func() {
		if err := runtime.DeleteSandbox(context.Background(), second); err != nil {
			t.Fatalf("delete second docker sandbox: %v", err)
		}
	}()

	if first.HostPort == "" || second.HostPort == "" {
		t.Fatalf("expected both sandboxes to receive host ports, got first=%#v second=%#v", first, second)
	}
	if first.HostPort == second.HostPort || first.EnvdURL == second.EnvdURL {
		t.Fatalf("expected dynamic host ports to be distinct, got first=%#v second=%#v", first, second)
	}
}

func TestDockerRuntimeRestoresManagedSandboxIntoApp(t *testing.T) {
	cfg := gateway.DefaultConfig()
	cfg.Runtime.Type = "docker"

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	runtime, err := NewDockerRuntime(cfg.Docker, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("docker runtime is unavailable: %v", err)
	}

	template := dockerIntegrationTemplate(t, runtime, ctx)
	skipUnlessDockerIntegrationEnvdAvailable(t, runtime, ctx, template.ImageRef)

	sandboxID := "sbx_restore_integration"
	containerName := cfg.Docker.ContainerNamePrefix + sandboxID
	_ = runtime.removeContainer(context.Background(), containerName)

	createdAt := time.Now().UTC()
	endAt := createdAt.Add(10 * time.Minute)
	info, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  sandboxID,
		TemplateID: template.TemplateID,
		Metadata:   map[string]string{"source": "restore-integration"},
		CreatedAt:  createdAt,
		EndAt:      endAt,
	})
	if err != nil {
		t.Fatalf("create docker sandbox: %v", err)
	}
	t.Cleanup(func() {
		_ = runtime.DeleteSandbox(context.Background(), info)
	})

	app, err := gateway.NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app with restored runtime: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sandboxID, nil)
	getReq = getReq.WithContext(ctx)
	getRec := httptest.NewRecorder()
	app.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected restored sandbox status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}

	var detail e2bapi.SandboxDetail
	if err := json.Unmarshal(getRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode restored sandbox detail: %v", err)
	}
	if detail.SandboxID != sandboxID || detail.TemplateID != template.TemplateID {
		t.Fatalf("unexpected restored sandbox detail: %#v", detail)
	}
	if detail.Metadata == nil || (*detail.Metadata)["source"] != "restore-integration" {
		t.Fatalf("expected restored metadata, got %#v", detail.Metadata)
	}
}

func TestDockerRuntimeGatewayCreatePauseConnectDelete(t *testing.T) {
	cfg := gateway.DefaultConfig()
	cfg.Runtime.Type = "docker"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runtime, err := NewDockerRuntime(cfg.Docker, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("docker runtime is unavailable: %v", err)
	}

	template := dockerIntegrationTemplate(t, runtime, ctx)
	skipUnlessDockerIntegrationEnvdAvailable(t, runtime, ctx, template.ImageRef)

	app, err := gateway.NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	createBody := bytes.NewBufferString(`{"templateID":` + strconv.Quote(template.TemplateID) + `}`)
	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", createBody)
	createReq = createReq.WithContext(ctx)
	createRec := httptest.NewRecorder()

	app.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var sandbox gateway.SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	containerName := cfg.Docker.ContainerNamePrefix + sandbox.SandboxID
	t.Cleanup(func() {
		_ = runtime.removeContainer(context.Background(), containerName)
	})

	inspect, err := runtime.client.ContainerInspect(ctx, containerName)
	if err != nil {
		t.Fatalf("inspect created container: %v", err)
	}

	if inspect.State == nil || !inspect.State.Running {
		t.Fatalf("expected created container to be running, got %#v", inspect.State)
	}

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/pause", nil)
	pauseReq = pauseReq.WithContext(ctx)
	pauseRec := httptest.NewRecorder()

	app.ServeHTTP(pauseRec, pauseReq)

	if pauseRec.Code != http.StatusOK {
		t.Fatalf("expected pause status %d, got %d: %s", http.StatusOK, pauseRec.Code, pauseRec.Body.String())
	}

	inspect, err = runtime.client.ContainerInspect(ctx, containerName)
	if err != nil {
		t.Fatalf("inspect paused container: %v", err)
	}

	if inspect.State == nil || !inspect.State.Paused {
		t.Fatalf("expected container to be paused, got %#v", inspect.State)
	}

	connectReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/connect", bytes.NewBufferString(`{}`))
	connectReq = connectReq.WithContext(ctx)
	connectRec := httptest.NewRecorder()

	app.ServeHTTP(connectRec, connectReq)

	if connectRec.Code != http.StatusOK {
		t.Fatalf("expected connect status %d, got %d: %s", http.StatusOK, connectRec.Code, connectRec.Body.String())
	}

	inspect, err = runtime.client.ContainerInspect(ctx, containerName)
	if err != nil {
		t.Fatalf("inspect resumed container: %v", err)
	}

	if inspect.State == nil || !inspect.State.Running || inspect.State.Paused {
		t.Fatalf("expected container to be running after connect, got %#v", inspect.State)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+sandbox.SandboxID, nil)
	deleteReq = deleteReq.WithContext(ctx)
	deleteRec := httptest.NewRecorder()

	app.ServeHTTP(deleteRec, deleteReq)

	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("expected delete status %d, got %d: %s", http.StatusNoContent, deleteRec.Code, deleteRec.Body.String())
	}

	if _, err := runtime.client.ContainerInspect(ctx, containerName); !errdefs.IsNotFound(err) {
		t.Fatalf("expected deleted container to be absent, got err=%v", err)
	}
}
