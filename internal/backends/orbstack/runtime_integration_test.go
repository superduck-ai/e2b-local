//go:build integration && darwin

package orbstackbackend

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"
)

const defaultOrbstackIntegrationBaseMachine = "ubuntu-2404"

func TestOrbstackRuntimeCreatePauseResumeDelete(t *testing.T) {
	cfg := orbstackIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runtime, err := NewOrbstackRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("orbstack runtime is unavailable: %v", err)
	}
	t.Cleanup(func() { cleanupOrbstackIntegrationMachines(t, cfg) })

	info, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  "sbx-orb-integration",
		TemplateID: orbstackIntegrationBaseMachine(),
		Metadata:   map[string]string{"source": "integration"},
	})
	if err != nil {
		t.Fatalf("create orbstack sandbox: %v", err)
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = runtime.DeleteSandbox(context.Background(), info)
		}
	})

	if info.ContainerID == "" || info.MachineID == "" || info.EnvdURL == "" || info.ContainerIP == "" {
		t.Fatalf("expected populated runtime info, got %#v", info)
	}
	assertHealthy(t, info.EnvdURL)

	if err := runtime.PauseSandbox(ctx, info); err != nil {
		t.Fatalf("pause orbstack sandbox: %v", err)
	}

	inspection, err := runtime.InspectSandbox(ctx, info)
	if err != nil {
		t.Fatalf("inspect paused sandbox: %v", err)
	}
	if !inspection.Exists || inspection.State != string(e2bapi.Paused) {
		t.Fatalf("expected paused sandbox inspection, got %#v", inspection)
	}

	resumed, err := runtime.ResumeSandbox(ctx, info)
	if err != nil {
		t.Fatalf("resume orbstack sandbox: %v", err)
	}
	if resumed.MachineID != info.MachineID {
		t.Fatalf("expected resumed sandbox to keep machine %q, got %#v", info.MachineID, resumed)
	}
	assertHealthy(t, resumed.EnvdURL)

	if err := runtime.DeleteSandbox(ctx, resumed); err != nil {
		t.Fatalf("delete orbstack sandbox: %v", err)
	}
	deleted = true

	inspection, err = runtime.InspectSandbox(ctx, resumed)
	if err != nil {
		t.Fatalf("inspect deleted sandbox: %v", err)
	}
	if inspection.Exists {
		t.Fatalf("expected deleted sandbox to be absent, got %#v", inspection)
	}
}

func TestOrbstackRuntimeVolumeMountPersistsAcrossSandboxes(t *testing.T) {
	cfg := orbstackIntegrationConfig(t)
	runOrbstackVolumeMountPersistenceTest(t, cfg)
}

func TestOrbstackRuntimeIsolatedVolumeMountPersistsAcrossSandboxes(t *testing.T) {
	cfg := orbstackIntegrationConfig(t)
	cfg.Isolated = true
	cfg.VolumeHostPath = t.TempDir()
	runOrbstackVolumeMountPersistenceTest(t, cfg)
}

func runOrbstackVolumeMountPersistenceTest(t *testing.T, cfg OrbstackRuntimeConfig) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	runtime, err := NewOrbstackRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("orbstack runtime is unavailable: %v", err)
	}
	t.Cleanup(func() { cleanupOrbstackIntegrationMachines(t, cfg) })

	volume, err := runtime.CreateVolume(ctx, "integration-data")
	if err != nil {
		t.Fatalf("create orbstack volume: %v", err)
	}
	volumeDeleted := false
	t.Cleanup(func() {
		if !volumeDeleted {
			_, _ = runtime.DeleteVolume(context.Background(), volume.VolumeID)
		}
	})

	first, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  "sbx-orb-volume-a",
		TemplateID: orbstackIntegrationBaseMachine(),
		VolumeMounts: []VolumeMount{
			{VolumeID: volume.VolumeID, Path: "/data"},
		},
	})
	if err != nil {
		t.Fatalf("create first sandbox with volume: %v", err)
	}
	firstDeleted := false
	t.Cleanup(func() {
		if !firstDeleted {
			_ = runtime.DeleteSandbox(context.Background(), first)
		}
	})
	if cfg.Isolated {
		vmInfo, err := runtime.vmClient.GetVMInfo(ctx, machineNameFromRuntimeInfo(first))
		if err != nil {
			t.Fatalf("inspect isolated sandbox machine: %v", err)
		}
		if !vmInfo.Config.Isolated {
			t.Fatalf("expected isolated sandbox machine config, got %#v", vmInfo)
		}
	}

	persistPath := isolatedVolumeSourcePath(volume.VolumeID) + "/persist.txt"
	if err := runtime.vmClient.WriteFile(ctx, machineNameFromRuntimeInfo(first), persistPath, []byte("volume-persist-ok"), 0o644); err != nil {
		t.Fatalf("write persisted volume data: %v", err)
	}

	if err := runtime.DeleteSandbox(ctx, first); err != nil {
		t.Fatalf("delete first sandbox with volume: %v", err)
	}
	firstDeleted = true

	second, err := runtime.CreateSandbox(ctx, SandboxRuntimeCreateRequest{
		SandboxID:  "sbx-orb-volume-b",
		TemplateID: orbstackIntegrationBaseMachine(),
		VolumeMounts: []VolumeMount{
			{VolumeID: volume.VolumeID, Path: "/data"},
		},
	})
	if err != nil {
		t.Fatalf("create second sandbox with volume: %v", err)
	}
	secondDeleted := false
	t.Cleanup(func() {
		if !secondDeleted {
			_ = runtime.DeleteSandbox(context.Background(), second)
		}
	})

	output, err := runtime.vmClient.ReadFile(ctx, machineNameFromRuntimeInfo(second), persistPath)
	if err != nil {
		t.Fatalf("read persisted volume data: %v", err)
	}
	if strings.TrimSpace(string(output)) != "volume-persist-ok" {
		t.Fatalf("expected persisted volume contents, got %q", output)
	}

	if err := runtime.DeleteSandbox(ctx, second); err != nil {
		t.Fatalf("delete second sandbox with volume: %v", err)
	}
	secondDeleted = true

	deleted, err := runtime.DeleteVolume(ctx, volume.VolumeID)
	if err != nil {
		t.Fatalf("delete orbstack volume: %v", err)
	}
	if !deleted {
		t.Fatal("expected delete volume to report success")
	}
	volumeDeleted = true
}

func orbstackIntegrationConfig(t *testing.T) OrbstackRuntimeConfig {
	t.Helper()

	cfg := gateway.DefaultConfig().Orbstack
	cfg.MachineNamePrefix = fmt.Sprintf("e2b-it-orb-%d-", time.Now().UTC().UnixNano())
	cfg.HealthTimeoutSeconds = 120

	if _, err := os.Stat(cfg.EnvdBinary); err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}

	client := NewVMClient(log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.GetVMInfo(ctx, orbstackIntegrationBaseMachine()); err != nil {
		t.Skipf("orbstack base machine %q is unavailable: %v", orbstackIntegrationBaseMachine(), err)
	}

	cleanupOrbstackIntegrationMachines(t, cfg)
	return cfg
}

func TestOrbstackRuntimeListTemplatesUsesCurrentMachines(t *testing.T) {
	cfg := orbstackIntegrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runtime, err := NewOrbstackRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("orbstack runtime is unavailable: %v", err)
	}

	templates, err := runtime.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("list orbstack templates: %v", err)
	}

	foundBaseMachine := false
	for _, template := range templates {
		if strings.HasPrefix(template.TemplateID, cfg.MachineNamePrefix) {
			t.Fatalf("expected internal sandbox machine to be filtered out, got %#v", template)
		}
		if template.TemplateID == orbstackIntegrationBaseMachine() {
			foundBaseMachine = true
		}
	}
	if !foundBaseMachine {
		t.Fatalf("expected template list to include %q, got %#v", orbstackIntegrationBaseMachine(), templates)
	}
}

func orbstackIntegrationBaseMachine() string {
	if value := strings.TrimSpace(os.Getenv("E2B_ORBSTACK_TEST_BASE_MACHINE")); value != "" {
		return value
	}
	return defaultOrbstackIntegrationBaseMachine
}

func cleanupOrbstackIntegrationMachines(t *testing.T, cfg OrbstackRuntimeConfig) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := NewVMClient(log.New(io.Discard, "", 0))
	vms, err := client.ListVMs(ctx)
	if err != nil {
		t.Logf("list orbstack machines for cleanup: %v", err)
		return
	}

	prefix := strings.TrimSpace(cfg.MachineNamePrefix)
	for _, vm := range vms {
		name := strings.TrimSpace(vm.Name)
		if name == "" || !strings.HasPrefix(name, prefix) {
			continue
		}
		if err := client.DeleteVM(context.Background(), name); err != nil {
			t.Logf("delete orbstack machine %q during cleanup: %v", name, err)
		}
	}
}

func assertHealthy(t *testing.T, envdURL string) {
	t.Helper()

	resp, err := http.Get(strings.TrimRight(envdURL, "/") + "/health")
	if err != nil {
		t.Fatalf("check envd health %s: %v", envdURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("expected healthy envd status for %s, got %d", envdURL, resp.StatusCode)
	}
}
