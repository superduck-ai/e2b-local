//go:build sbx_runtime_integration && darwin

package sbxbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"
)

func TestSbxRuntimeCreateExecPTYMetricsPauseResumeDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime := newSbxIntegrationRuntime(t, ctx)
	activeRuntime := runtime
	runtime.checkHealthy = func(context.Context, string) error { return nil }

	createdAt := time.Now().UTC()
	info, err := runtime.CreateSandbox(ctx, gateway.SandboxRuntimeCreateRequest{
		SandboxID:  fmt.Sprintf("integration-%d", time.Now().UnixNano()),
		TemplateID: "sbx",
		Metadata:   map[string]string{"source": "sbx-runtime-integration"},
		EnvVars:    map[string]string{"E2B_SBX_INTEGRATION": "true"},
		CreatedAt:  createdAt,
		EndAt:      createdAt.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create sandbox through sandboxd: %v", err)
	}
	t.Cleanup(func() {
		if deleteErr := activeRuntime.DeleteSandbox(context.Background(), info); deleteErr != nil {
			t.Errorf("delete sbx integration sandbox: %v", deleteErr)
		}
	})
	if info.ContainerID == "" || info.ContainerName == "" || info.MachineID == "" {
		t.Fatalf("expected sandboxd runtime identity, got %#v", info)
	}
	if info.EnvdURL == "" {
		t.Fatalf("expected reverse-tunnel envd URL, got %#v", info)
	}
	t.Logf("created sandbox=%s container=%s envd=%s", info.SandboxID, info.ContainerID, info.EnvdURL)

	if err := runtime.defaultHealthCheck(ctx, info.EnvdURL); err != nil {
		logs, logErr := runtime.runSandboxCommand(context.Background(), info, []string{"/bin/sh", "-lc", "tail -n 80 /var/log/envd.log 2>/dev/null || true"})
		t.Fatalf("reach envd through reverse tunnel: %v; guest logs=%q; log error=%v", err, logs, logErr)
	}
	runtime.checkHealthy = runtime.defaultHealthCheck

	response, err := http.Get(info.EnvdURL + "/health")
	if err != nil {
		t.Fatalf("reach envd through reverse tunnel: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("expected envd health success, got %d", response.StatusCode)
	}

	output, err := runtime.runSandboxCommand(ctx, info, []string{"/bin/sh", "-lc", "printf sandboxd-exec-ok"})
	if err != nil {
		t.Fatalf("run command through sandboxd: %v", err)
	}
	if !strings.Contains(output, "sandboxd-exec-ok") {
		t.Fatalf("unexpected sandboxd exec output %q", output)
	}

	attached, execID, err := runtime.OpenPTY(ctx, info, []string{"/bin/sh", "-lc", "printf docker-pty-ok; sleep 1"})
	if err != nil {
		t.Fatalf("open Docker hijacked PTY: %v", err)
	}
	defer attached.Close()
	if err := runtime.ResizePTY(ctx, execID, 40, 120); err != nil {
		t.Fatalf("resize Docker PTY: %v", err)
	}
	ptyOutput, err := io.ReadAll(attached.Reader)
	if err != nil {
		t.Fatalf("read Docker PTY: %v", err)
	}
	if !strings.Contains(string(ptyOutput), "docker-pty-ok") {
		t.Fatalf("unexpected Docker PTY output %q", ptyOutput)
	}

	metrics, err := runtime.GetSandboxMetrics(ctx, gateway.SandboxRecord{
		ID:          info.SandboxID,
		RuntimeInfo: info,
		CreatedAt:   createdAt,
		EndAt:       createdAt.Add(10 * time.Minute),
	}, gateway.SandboxMetricsRequest{})
	if err != nil {
		t.Fatalf("read metrics socket: %v", err)
	}
	if len(metrics) != 1 || metrics[0].MemTotal <= 0 {
		t.Fatalf("expected parsed sbx metrics, got %#v", metrics)
	}

	if _, err := runtime.GetSandboxLogs(ctx, info, gateway.SandboxLogsRequest{Limit: 20}); err != nil {
		t.Fatalf("read sbx logs through sandboxd exec: %v", err)
	}

	// A fresh runtime instance represents a gateway restart. sandboxd does not
	// retain the bootstrap environment, so this validates the local state file
	// and relay recreation while the sandbox is still running.
	runtime.closeRelays(info.ContainerID)
	runtime = newSbxIntegrationRuntime(t, ctx)
	activeRuntime = runtime
	restored, err := runtime.RestoreSandboxes(ctx)
	if err != nil {
		t.Fatalf("restore running sandbox after gateway restart: %v", err)
	}
	restoredRecord := sbxIntegrationRecord(t, restored, info.SandboxID)
	if restoredRecord.State != string(e2bapi.Running) {
		t.Fatalf("expected restored running sandbox, got %#v", restoredRecord)
	}
	info = restoredRecord.RuntimeInfo
	if err := runtime.defaultHealthCheck(ctx, info.EnvdURL); err != nil {
		t.Fatalf("reach envd after running-sandbox restore: %v", err)
	}

	if err := runtime.PauseSandbox(ctx, info); err != nil {
		t.Fatalf("pause sandbox through sandboxd: %v", err)
	}
	if err := waitForSbxState(ctx, runtime, info, "paused"); err != nil {
		t.Fatal(err)
	}

	// A paused sandbox has no active guest tunnel. Restart the gateway before
	// resuming to ensure ResumeSandbox rebinds its relay from persisted state.
	runtime.closeRelays(info.ContainerID)
	runtime = newSbxIntegrationRuntime(t, ctx)
	activeRuntime = runtime
	restored, err = runtime.RestoreSandboxes(ctx)
	if err != nil {
		t.Fatalf("restore paused sandbox after gateway restart: %v", err)
	}
	restoredRecord = sbxIntegrationRecord(t, restored, info.SandboxID)
	if restoredRecord.State != string(e2bapi.Paused) {
		t.Fatalf("expected restored paused sandbox, got %#v", restoredRecord)
	}
	info = restoredRecord.RuntimeInfo
	resumed, err := runtime.ResumeSandbox(ctx, info)
	if err != nil {
		t.Fatalf("resume sandbox through sandboxd: %v", err)
	}
	info = resumed
	if err := waitForSbxState(ctx, runtime, resumed, "running"); err != nil {
		t.Fatal(err)
	}
	output, err = runtime.runSandboxCommand(ctx, resumed, []string{"/bin/sh", "-lc", "printf resume-bootstrap-ok"})
	if err != nil {
		t.Fatalf("run command after paused restore and resume: %v", err)
	}
	if !strings.Contains(output, "resume-bootstrap-ok") {
		t.Fatalf("unexpected resume command output %q", output)
	}
}

func TestSbxRuntimeVolumePersistence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime := newSbxIntegrationRuntime(t, ctx)
	createdAt := time.Now().UTC()
	volumeName := fmt.Sprintf("sbx-integration-volume-%d", time.Now().UnixNano())
	volume, err := runtime.CreateVolume(ctx, volumeName)
	if err != nil {
		t.Fatalf("create sbx volume: %v", err)
	}
	t.Cleanup(func() {
		if _, deleteErr := runtime.DeleteVolume(context.Background(), volume.VolumeID); deleteErr != nil {
			t.Errorf("delete sbx integration volume: %v", deleteErr)
		}
	})
	if _, err := runtime.WriteVolumeFile(ctx, volume.VolumeID, "manifest.txt", strings.NewReader("volume-before-sandbox"), gateway.VolumeWriteOptions{Force: true}); err != nil {
		t.Fatalf("write sbx volume file: %v", err)
	}

	mounts := []gateway.VolumeMount{{VolumeID: volume.VolumeID, Path: "/mnt/e2b-data"}}
	first, err := runtime.CreateSandbox(ctx, gateway.SandboxRuntimeCreateRequest{
		SandboxID:    fmt.Sprintf("integration-volume-first-%d", time.Now().UnixNano()),
		TemplateID:   "sbx",
		VolumeMounts: mounts,
		CreatedAt:    createdAt,
		EndAt:        createdAt.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create first mounted sbx sandbox: %v", err)
	}
	firstAlive := true
	t.Cleanup(func() {
		if firstAlive {
			if deleteErr := runtime.DeleteSandbox(context.Background(), first); deleteErr != nil {
				t.Errorf("delete first mounted sbx sandbox: %v", deleteErr)
			}
		}
	})
	output, err := runtime.runSandboxCommand(ctx, first, []string{"/bin/sh", "-lc", "cat /mnt/e2b-data/manifest.txt"})
	if err != nil {
		t.Fatalf("read mounted sbx volume: %v", err)
	}
	if !strings.Contains(output, "volume-before-sandbox") {
		t.Fatalf("unexpected mounted volume content %q", output)
	}
	if _, err := runtime.runSandboxCommand(ctx, first, []string{"/bin/sh", "-lc", "printf volume-after-sandbox > /mnt/e2b-data/persist.txt"}); err != nil {
		t.Fatalf("write mounted sbx volume: %v", err)
	}

	if err := runtime.DeleteSandbox(ctx, first); err != nil {
		t.Fatalf("delete first mounted sbx sandbox: %v", err)
	}
	firstAlive = false
	second, err := runtime.CreateSandbox(ctx, gateway.SandboxRuntimeCreateRequest{
		SandboxID:    fmt.Sprintf("integration-volume-second-%d", time.Now().UnixNano()),
		TemplateID:   "sbx",
		VolumeMounts: mounts,
		CreatedAt:    time.Now().UTC(),
		EndAt:        time.Now().UTC().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create second mounted sbx sandbox: %v", err)
	}
	t.Cleanup(func() {
		if deleteErr := runtime.DeleteSandbox(context.Background(), second); deleteErr != nil {
			t.Errorf("delete second mounted sbx sandbox: %v", deleteErr)
		}
	})
	output, err = runtime.runSandboxCommand(ctx, second, []string{"/bin/sh", "-lc", "cat /mnt/e2b-data/persist.txt"})
	if err != nil {
		t.Fatalf("read persisted sbx volume: %v", err)
	}
	if !strings.Contains(output, "volume-after-sandbox") {
		t.Fatalf("unexpected persisted volume content %q", output)
	}
}

func TestSbxRuntimeGatewayCreatePauseConnectDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runtime := newSbxIntegrationRuntime(t, ctx)
	cfg := gateway.DefaultConfig()
	cfg.Runtime.Type = "sbx"
	cfg.Sbx = runtime.cfg
	app, err := gateway.NewAppWithRuntime(cfg, log.New(io.Discard, "", 0), runtime)
	if err != nil {
		t.Fatalf("create sbx gateway app: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewBufferString(`{"templateID":"sbx"}`)).WithContext(ctx)
	createRec := httptest.NewRecorder()
	app.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create sbx sandbox through gateway: status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var sandbox gateway.SandboxResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode sbx gateway sandbox: %v", err)
	}
	if sandbox.SandboxID == "" || sandbox.EnvdURL == "" {
		t.Fatalf("unexpected sbx gateway sandbox: %#v", sandbox)
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = runtime.DeleteSandbox(context.Background(), gateway.SandboxRuntimeInfo{
				SandboxID:     sandbox.SandboxID,
				ContainerName: sbxSandboxName(sandbox.SandboxID),
				MachineID:     sbxSandboxName(sandbox.SandboxID),
			})
		}
	})

	snapshotReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/snapshots", bytes.NewBufferString(`{"name":"savepoint"}`)).WithContext(ctx)
	snapshotRec := httptest.NewRecorder()
	app.ServeHTTP(snapshotRec, snapshotReq)
	if snapshotRec.Code != http.StatusNotImplemented || !strings.Contains(snapshotRec.Body.String(), "snapshots are not supported") {
		t.Fatalf("SBX snapshot must be explicitly unsupported: status=%d body=%s", snapshotRec.Code, snapshotRec.Body.String())
	}

	listSnapshotsReq := httptest.NewRequest(http.MethodGet, "/snapshots?sandboxID="+sandbox.SandboxID, nil).WithContext(ctx)
	listSnapshotsRec := httptest.NewRecorder()
	app.ServeHTTP(listSnapshotsRec, listSnapshotsReq)
	if listSnapshotsRec.Code != http.StatusNotImplemented || !strings.Contains(listSnapshotsRec.Body.String(), "snapshots are not supported") {
		t.Fatalf("SBX snapshot listing must be explicitly unsupported: status=%d body=%s", listSnapshotsRec.Code, listSnapshotsRec.Body.String())
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sandbox.SandboxID+"/metrics", nil).WithContext(ctx)
	metricsRec := httptest.NewRecorder()
	app.ServeHTTP(metricsRec, metricsReq)
	if metricsRec.Code != http.StatusOK {
		t.Fatalf("read sbx gateway metrics: status=%d body=%s", metricsRec.Code, metricsRec.Body.String())
	}

	pauseReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/pause", nil).WithContext(ctx)
	pauseRec := httptest.NewRecorder()
	app.ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusNoContent {
		t.Fatalf("pause sbx sandbox through gateway: status=%d body=%s", pauseRec.Code, pauseRec.Body.String())
	}

	connectReq := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandbox.SandboxID+"/connect", bytes.NewBufferString(`{}`)).WithContext(ctx)
	connectRec := httptest.NewRecorder()
	app.ServeHTTP(connectRec, connectReq)
	if connectRec.Code != http.StatusCreated {
		t.Fatalf("connect paused sbx sandbox through gateway: status=%d body=%s", connectRec.Code, connectRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+sandbox.SandboxID, nil).WithContext(ctx)
	deleteRec := httptest.NewRecorder()
	app.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete sbx sandbox through gateway: status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	deleted = true
}

func sbxIntegrationRecord(t *testing.T, records []gateway.SandboxRecord, sandboxID string) gateway.SandboxRecord {
	t.Helper()
	for _, record := range records {
		if record.ID == sandboxID {
			return record
		}
	}
	t.Fatalf("sandbox %q was not restored: %#v", sandboxID, records)
	return gateway.SandboxRecord{}
}

func newSbxIntegrationRuntime(t *testing.T, ctx context.Context) *SbxRuntime {
	t.Helper()
	cfg := gateway.DefaultConfig().Sbx
	cfg.TunnelPortRange = nil
	cfg.TunnelConnections = 1
	cfg.HealthTimeoutSeconds = 12
	runtime, err := NewSbxRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("create sbx runtime: %v", err)
	}
	if _, err := os.Stat(runtime.cfg.SandboxdSocket); err != nil {
		t.Skipf("sandboxd socket is unavailable: %v", err)
	}
	if _, err := os.Stat(runtime.cfg.DockerSocket); err != nil {
		t.Skipf("sbx Docker socket is unavailable: %v", err)
	}
	loggedIn, err := runtime.isLoggedIn(ctx)
	if err != nil {
		t.Skipf("sbx login state is unavailable: %v", err)
	}
	if !loggedIn {
		t.Skip("sbx is not logged in; run sbx login before the integration test")
	}
	templates, err := runtime.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("list sbx templates: %v", err)
	}
	for _, template := range templates {
		if template.TemplateID == "sbx" && template.ImageRef == runtime.cfg.DefaultImage {
			return runtime
		}
	}
	t.Skipf("sbx image %q is unavailable; run scripts/build-sbx-image.sh", runtime.cfg.DefaultImage)
	return nil
}

func waitForSbxState(ctx context.Context, runtime *SbxRuntime, info gateway.SandboxRuntimeInfo, want string) error {
	for {
		inspection, err := runtime.InspectSandbox(ctx, info)
		if err != nil {
			return fmt.Errorf("inspect sandbox while waiting for %s: %w", want, err)
		}
		if inspection.Exists && inspection.State == want {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("sandbox did not reach %s: %w", want, ctx.Err())
		}
		time.Sleep(200 * time.Millisecond)
	}
}
