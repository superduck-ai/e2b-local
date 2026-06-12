//go:build integration && darwin && cgo

package applecontainer

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	gateway "e2b-local/internal/gateway"
)

const defaultAppleContainerIntegrationImage = "docker.io/library/alpine:3.20"

func TestAppleContainerRuntimeCreatePauseResumeDelete(t *testing.T) {
	cfg := appleContainerIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	runtime := appleContainerIntegrationRuntime(t, cfg, ctx)
	sandboxID := "sbx-apple-integration"
	containerID := appleSandboxContainerID(cfg, sandboxID)
	cleanupCtx, cleanupCancel := appleContainerIntegrationCleanupContext()
	_ = runtime.DeleteSandbox(cleanupCtx, gateway.SandboxRuntimeInfo{ContainerID: containerID})
	cleanupCancel()

	info, err := runtime.CreateSandbox(ctx, gateway.SandboxRuntimeCreateRequest{
		SandboxID:  sandboxID,
		TemplateID: appleContainerIntegrationTemplateID(),
		Metadata:   map[string]string{"source": "integration"},
	})
	if err != nil {
		t.Fatalf("create apple container sandbox: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			cleanupCtx, cleanupCancel := appleContainerIntegrationCleanupContext()
			defer cleanupCancel()
			_ = runtime.DeleteSandbox(cleanupCtx, info)
		}
	})

	if info.ContainerID != containerID || info.EnvdURL == "" || info.HostPort == "" {
		t.Fatalf("expected populated runtime info, got %#v", info)
	}

	runningResumed, err := runtime.ResumeSandbox(ctx, info)
	if err != nil {
		t.Fatalf("resume already-running apple container sandbox: %v", err)
	}
	if runningResumed.HostPort != info.HostPort || runningResumed.EnvdURL != info.EnvdURL {
		t.Fatalf("expected running resume to reuse host port, before=%#v after=%#v", info, runningResumed)
	}

	if err := runtime.PauseSandbox(ctx, info); err != nil {
		t.Fatalf("pause apple container sandbox: %v", err)
	}

	inspection, err := runtime.InspectSandbox(ctx, info)
	if err != nil {
		t.Fatalf("inspect paused apple container sandbox: %v", err)
	}
	if !inspection.Exists || inspection.State != "paused" {
		t.Fatalf("expected paused sandbox inspection, got %#v", inspection)
	}

	resumed, err := runtime.ResumeSandbox(ctx, info)
	if err != nil {
		t.Fatalf("resume apple container sandbox: %v", err)
	}
	if resumed.HostPort != info.HostPort || resumed.EnvdURL != info.EnvdURL {
		t.Fatalf("expected resume to reuse host port, before=%#v after=%#v", info, resumed)
	}

	if err := runtime.DeleteSandbox(ctx, resumed); err != nil {
		t.Fatalf("delete apple container sandbox: %v", err)
	}
	deleted = true
	if err := runtime.DeleteSandbox(ctx, resumed); err != nil {
		t.Fatalf("delete missing apple container sandbox should be idempotent: %v", err)
	}

	inspection, err = runtime.InspectSandbox(ctx, resumed)
	if err != nil {
		t.Fatalf("inspect deleted apple container sandbox: %v", err)
	}
	if inspection.Exists {
		t.Fatalf("expected deleted sandbox to be absent, got %#v", inspection)
	}
}

func TestAppleContainerRuntimeVolumeMountCreateAndRestore(t *testing.T) {
	cfg := appleContainerIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	runtime := appleContainerIntegrationRuntime(t, cfg, ctx)
	volumeName := fmt.Sprintf("apple-integration-data-%d", time.Now().UTC().UnixNano())
	volume, err := runtime.CreateVolume(ctx, volumeName)
	if err != nil {
		t.Fatalf("create apple container volume: %v", err)
	}
	volumeDeleted := false
	t.Cleanup(func() {
		if !volumeDeleted {
			cleanupCtx, cleanupCancel := appleContainerIntegrationCleanupContext()
			defer cleanupCancel()
			_, _ = runtime.DeleteVolume(cleanupCtx, volume.VolumeID)
		}
	})

	sandboxID := "sbx-apple-volume"
	containerID := appleSandboxContainerID(cfg, sandboxID)
	cleanupCtx, cleanupCancel := appleContainerIntegrationCleanupContext()
	_ = runtime.DeleteSandbox(cleanupCtx, gateway.SandboxRuntimeInfo{ContainerID: containerID})
	cleanupCancel()

	createdAt := time.Now().UTC()
	endAt := createdAt.Add(10 * time.Minute)
	info, err := runtime.CreateSandbox(ctx, gateway.SandboxRuntimeCreateRequest{
		SandboxID:  sandboxID,
		TemplateID: appleContainerIntegrationTemplateID(),
		Metadata:   map[string]string{"source": "restore-integration"},
		CreatedAt:  createdAt,
		EndAt:      endAt,
		VolumeMounts: []gateway.VolumeMount{{
			VolumeID:  volume.VolumeID,
			MountPath: "/mnt/data",
		}},
	})
	if err != nil {
		t.Fatalf("create apple container sandbox with volume: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := appleContainerIntegrationCleanupContext()
		defer cleanupCancel()
		_ = runtime.DeleteSandbox(cleanupCtx, info)
	})

	if len(info.VolumeMounts) != 1 || info.VolumeMounts[0].MountPath != "/mnt/data" {
		t.Fatalf("expected mounted volume in runtime info, got %#v", info.VolumeMounts)
	}

	records, err := runtime.RestoreSandboxes(ctx)
	if err != nil {
		t.Fatalf("restore apple container sandboxes: %v", err)
	}
	record, ok := restoredRecordByID(records, sandboxID)
	if !ok {
		t.Fatalf("expected restored sandbox %s in %#v", sandboxID, records)
	}
	if record.RuntimeInfo.HostPort != info.HostPort || record.RuntimeInfo.EnvdURL != info.EnvdURL {
		t.Fatalf("expected restored runtime info to keep envd host port, record=%#v info=%#v", record.RuntimeInfo, info)
	}
	if record.Metadata["source"] != "restore-integration" || !record.EndAt.Equal(endAt) {
		t.Fatalf("expected metadata and endAt to restore, got %#v", record)
	}
	if len(record.RuntimeInfo.VolumeMounts) != 1 || record.RuntimeInfo.VolumeMounts[0].MountPath != "/mnt/data" {
		t.Fatalf("expected restored volume mounts, got %#v", record.RuntimeInfo.VolumeMounts)
	}

	if err := runtime.DeleteSandbox(ctx, info); err != nil {
		t.Fatalf("delete apple container volume sandbox: %v", err)
	}
	deleted, err := runtime.DeleteVolume(ctx, volume.VolumeID)
	if err != nil {
		t.Fatalf("delete apple container volume: %v", err)
	}
	if !deleted {
		t.Fatal("expected delete volume to report success")
	}
	volumeDeleted = true
}

func appleContainerIntegrationCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func appleContainerIntegrationConfig(t *testing.T) AppleContainerRuntimeConfig {
	t.Helper()

	cfg := gateway.DefaultConfig().AppleContainer
	cfg.ContainerNamePrefix = fmt.Sprintf("e2b-it-apple-%d-", time.Now().UTC().UnixNano())
	cfg.HealthTimeoutSeconds = 120
	cfg.Templates = map[string]AppleContainerTemplateConfig{
		appleContainerIntegrationTemplateID(): {
			Image: appleContainerIntegrationImage(),
		},
	}

	if value := strings.TrimSpace(os.Getenv("E2B_APPLECONTAINER_TEST_ENVD_BINARY")); value != "" {
		cfg.EnvdBinary = value
	}
	if _, err := os.Stat(cfg.EnvdBinary); err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}
	return cfg
}

func appleContainerIntegrationRuntime(t *testing.T, cfg AppleContainerRuntimeConfig, ctx context.Context) *AppleContainerRuntime {
	t.Helper()

	runtime, err := NewAppleContainerRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("apple container runtime is unavailable: %v", err)
	}
	t.Cleanup(runtime.Close)

	if err := runtime.client.Ping(ctx); err != nil {
		t.Skipf("apple container apiserver is unavailable: %v", err)
	}
	if _, err := runtime.client.ResolveImage(ctx, appleContainerIntegrationImage()); err != nil {
		t.Skipf("apple container image %q is unavailable: %v", appleContainerIntegrationImage(), err)
	}
	return runtime
}

func appleContainerIntegrationTemplateID() string {
	if value := strings.TrimSpace(os.Getenv("E2B_APPLECONTAINER_TEST_TEMPLATE_ID")); value != "" {
		return value
	}
	return "apple-integration"
}

func appleContainerIntegrationImage() string {
	if value := strings.TrimSpace(os.Getenv("E2B_APPLECONTAINER_TEST_IMAGE")); value != "" {
		return value
	}
	return defaultAppleContainerIntegrationImage
}

func restoredRecordByID(records []gateway.SandboxRecord, sandboxID string) (gateway.SandboxRecord, bool) {
	for _, record := range records {
		if record.ID == sandboxID {
			return record, true
		}
	}
	return gateway.SandboxRecord{}, false
}
