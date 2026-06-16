//go:build go_sdk_integration

package sdk_integration_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gateway "e2b-local/internal/gateway"

	e2b "github.com/superduck-ai/e2b-go-sdk"
)

func TestGoSDKGatewayMVP(t *testing.T) {
	cfg := goSDKIntegrationConfig(t)
	skipUnlessGoSDKRuntimeAvailable(t, cfg)

	runGoSDKSandboxLifecycle(t, cfg, "go-sdk-gateway-ok")
}

func TestGoSDKGatewayFilesystemDirectEnvd(t *testing.T) {
	cfg := goSDKIntegrationConfig(t)
	skipUnlessGoSDKRuntimeAvailable(t, cfg)

	server := httptest.NewServer(gateway.NewApp(cfg, log.New(io.Discard, "", 0)))
	t.Cleanup(server.Close)

	t.Setenv("E2B_API_URL", server.URL)
	t.Setenv("E2B_SANDBOX_URL", "")
	t.Setenv("E2B_API_KEY", testAPIKey)
	t.Setenv("E2B_DEBUG", "false")

	ctx, cancel := context.WithTimeout(context.Background(), goSDKOperationTimeout(cfg, 60*time.Second))
	defer cancel()

	sandbox, err := e2b.Create(ctx, goSDKTemplateID(t, cfg), nil)
	if err != nil {
		t.Fatalf("create sandbox with Go SDK: %v", err)
	}
	t.Cleanup(func() {
		if err := sandbox.Kill(context.Background(), nil); err != nil {
			t.Fatalf("kill sandbox with Go SDK: %v", err)
		}
	})

	filename := fmt.Sprintf("e2b-local-filesystem-direct-%d.txt", time.Now().UnixNano())
	filePath := "/tmp/" + filename
	content := "go-sdk-filesystem-direct-ok"
	if _, err := sandbox.Files.Write(ctx, filePath, content, nil); err != nil {
		t.Fatalf("write file through direct envd URL: %v", err)
	}

	readValue, err := sandbox.Files.Read(ctx, filePath, nil)
	if err != nil {
		t.Fatalf("read file through direct envd URL: %v", err)
	}
	readText, ok := readValue.(string)
	if !ok {
		t.Fatalf("expected filesystem read to return string, got %T", readValue)
	}
	if readText != content {
		t.Fatalf("expected filesystem read content %q, got %q", content, readText)
	}

	entries, err := sandbox.Files.List(ctx, "/tmp", nil)
	if err != nil {
		t.Fatalf("list files through direct envd URL: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == filename || entry.Path == filePath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected /tmp list to include %q, got %#v", filePath, entries)
	}

}

func runGoSDKSandboxLifecycle(t *testing.T, cfg gateway.Config, marker string) {
	t.Helper()

	handler := gateway.NewApp(cfg, log.New(io.Discard, "", 0))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("E2B_API_URL", server.URL)
	t.Setenv("E2B_API_KEY", testAPIKey)
	t.Setenv("E2B_DEBUG", "false")
	t.Setenv("E2B_SANDBOX_URL", "")

	ctx, cancel := context.WithTimeout(context.Background(), goSDKOperationTimeout(cfg, 45*time.Second))
	defer cancel()

	templateID := goSDKTemplateID(t, cfg)

	templates, err := e2b.ListTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("list templates with Go SDK: %v", err)
	}
	if !templateInfoListContains(templates, templateID) {
		t.Fatalf("expected template list to contain %q, got %#v", templateID, templates)
	}

	sandbox, err := e2b.Create(ctx, templateID, nil)
	if err != nil {
		t.Fatalf("create sandbox with Go SDK: %v", err)
	}

	if sandbox.SandboxID == "" {
		t.Fatal("expected sandbox id from gateway")
	}
	t.Cleanup(func() {
		if err := sandbox.Kill(context.Background(), nil); err != nil {
			t.Fatalf("kill sandbox with Go SDK: %v", err)
		}
	})

	sandboxes, err := e2b.List(&e2b.SandboxListOpts{Limit: 10}).NextItems()
	if err != nil {
		t.Fatalf("list sandboxes with Go SDK: %v", err)
	}

	if !sandboxInfoListContains(sandboxes, sandbox.SandboxID) {
		t.Fatalf("expected list to contain sandbox %s, got %#v", sandbox.SandboxID, sandboxes)
	}

	paused, err := sandbox.Pause(ctx, nil)
	if err != nil {
		t.Fatalf("pause sandbox with Go SDK: %v", err)
	}

	if !paused {
		t.Fatal("expected Go SDK pause to report true")
	}

	pausedSandboxes, err := e2b.List(&e2b.SandboxListOpts{Limit: 10}).NextItems()
	if err != nil {
		t.Fatalf("list paused sandboxes with Go SDK: %v", err)
	}

	if sandboxInfoState(pausedSandboxes, sandbox.SandboxID) != e2b.SandboxState("paused") {
		t.Fatalf("expected sandbox %s to be paused, got %#v", sandbox.SandboxID, pausedSandboxes)
	}

	connected, err := e2b.Connect(ctx, sandbox.SandboxID, nil)
	if err != nil {
		t.Fatalf("connect sandbox with Go SDK: %v", err)
	}

	execution, err := connected.Commands.Run(ctx, fmt.Sprintf("echo %q", marker), nil)
	if err != nil {
		t.Fatalf("run command with Go SDK: %v", err)
	}

	result, ok := execution.(*e2b.CommandResult)
	if !ok {
		t.Fatalf("expected *e2b.CommandResult, got %T", execution)
	}

	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", result.ExitCode, result.Stderr)
	}

	if !strings.Contains(result.Stdout, marker) {
		t.Fatalf("expected stdout to contain marker, got %q", result.Stdout)
	}
}

func TestGoSDKGatewayVolumeLifecycle(t *testing.T) {
	cfg, err := gateway.LoadConfig(filepath.Join(repoRoot(t), "config.yaml"))
	if err != nil {
		t.Fatalf("load gateway config: %v", err)
	}
	if cfg.Runtime.Type != "docker" {
		t.Skip("volume lifecycle integration requires docker runtime")
	}
	skipUnlessDockerRuntimeAvailable(t, cfg)
	server := httptest.NewServer(gateway.NewApp(cfg, log.New(io.Discard, "", 0)))
	t.Cleanup(server.Close)

	t.Setenv("E2B_API_URL", server.URL)
	t.Setenv("E2B_SANDBOX_URL", "")
	t.Setenv("E2B_API_KEY", testAPIKey)
	t.Setenv("E2B_DEBUG", "false")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	templateID := goSDKTemplateID(t, cfg)
	volumeName := fmt.Sprintf("e2b-local-go-sdk-%d", time.Now().UnixNano())

	volume, err := e2b.CreateVolume(ctx, volumeName, nil)
	if err != nil {
		t.Fatalf("create volume with Go SDK: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e2b.DestroyVolume(context.Background(), volume.VolumeID, nil)
	})

	if volume.VolumeID != volumeName || volume.Name != volumeName {
		t.Fatalf("unexpected created volume: %#v", volume)
	}

	volumes, err := e2b.ListVolumes(ctx, nil)
	if err != nil {
		t.Fatalf("list volumes with Go SDK: %v", err)
	}
	if !volumeInfoListContains(volumes, volume.VolumeID) {
		t.Fatalf("expected list to contain volume %s, got %#v", volume.VolumeID, volumes)
	}

	info, err := e2b.GetVolumeInfo(ctx, volume.VolumeID, nil)
	if err != nil {
		t.Fatalf("get volume info with Go SDK: %v", err)
	}
	if info.VolumeID != volume.VolumeID || info.Name != volume.Name {
		t.Fatalf("unexpected volume info: %#v", info)
	}

	firstSandbox, err := e2b.Create(ctx, templateID, &e2b.SandboxOpts{
		VolumeMounts: map[string]any{
			"/mnt/data": volume,
		},
	})
	if err != nil {
		t.Fatalf("create first mounted sandbox: %v", err)
	}

	writeExecution, err := firstSandbox.Commands.Run(ctx, `set -eu; printf go-sdk-volume-ok > /mnt/data/persist.txt; cat /mnt/data/persist.txt`, nil)
	if err != nil {
		t.Fatalf("write mounted volume file: %v", err)
	}
	writeResult, ok := writeExecution.(*e2b.CommandResult)
	if !ok {
		t.Fatalf("expected *e2b.CommandResult, got %T", writeExecution)
	}
	if writeResult.ExitCode != 0 || !strings.Contains(writeResult.Stdout, "go-sdk-volume-ok") {
		t.Fatalf("unexpected write result: %#v", writeResult)
	}

	if err := firstSandbox.Kill(ctx, nil); err != nil {
		t.Fatalf("kill first mounted sandbox: %v", err)
	}

	secondSandbox, err := e2b.Create(ctx, templateID, &e2b.SandboxOpts{
		VolumeMounts: map[string]any{
			"/mnt/data": volume.Name,
		},
	})
	if err != nil {
		t.Fatalf("create second mounted sandbox: %v", err)
	}
	defer func() {
		if err := secondSandbox.Kill(context.Background(), nil); err != nil {
			t.Fatalf("kill second mounted sandbox: %v", err)
		}
	}()

	readExecution, err := secondSandbox.Commands.Run(ctx, `cat /mnt/data/persist.txt`, nil)
	if err != nil {
		t.Fatalf("read mounted volume file: %v", err)
	}
	readResult, ok := readExecution.(*e2b.CommandResult)
	if !ok {
		t.Fatalf("expected *e2b.CommandResult, got %T", readExecution)
	}
	if readResult.ExitCode != 0 || !strings.Contains(readResult.Stdout, "go-sdk-volume-ok") {
		t.Fatalf("unexpected read result: %#v", readResult)
	}
}

func volumeInfoListContains(volumes []e2b.VolumeInfo, volumeID string) bool {
	for _, volume := range volumes {
		if volume.VolumeID == volumeID {
			return true
		}
	}
	return false
}

func sandboxInfoListContains(sandboxes []e2b.SandboxInfo, sandboxID string) bool {
	for _, sandbox := range sandboxes {
		if sandbox.SandboxID == sandboxID {
			return true
		}
	}
	return false
}

func templateInfoListContains(templates []e2b.TemplateInfo, templateID string) bool {
	for _, template := range templates {
		if template.TemplateID == templateID {
			return true
		}
	}
	return false
}

func sandboxInfoState(sandboxes []e2b.SandboxInfo, sandboxID string) e2b.SandboxState {
	for _, sandbox := range sandboxes {
		if sandbox.SandboxID == sandboxID {
			return sandbox.State
		}
	}
	return ""
}
