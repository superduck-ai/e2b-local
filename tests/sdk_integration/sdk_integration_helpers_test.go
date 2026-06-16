//go:build go_sdk_integration || js_sdk_integration

package sdk_integration_test

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	_ "e2b-local/internal/backends/applecontainer"
	_ "e2b-local/internal/backends/docker"
	_ "e2b-local/internal/backends/orbstack"
	gateway "e2b-local/internal/gateway"
	"e2b-local/internal/orbctl"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

const testAPIKey = "e2b_0000000000000000000000000000000000000000"
const defaultOrbstackTestBaseMachine = "ubuntu-2404"
const dockerEnvdBinaryAMD64 = "envd-bin/envd-linux-amd64"
const dockerEnvdBinaryARM64 = "envd-bin/envd-linux-arm64"
const appleContainerSDKTimeout = 4 * time.Minute

func goSDKIntegrationConfig(t *testing.T) gateway.Config {
	t.Helper()

	cfg, err := gateway.LoadConfig(filepath.Join(repoRoot(t), "config.yaml"))
	if err != nil {
		t.Fatalf("load gateway config: %v", err)
	}
	return cfg
}

func skipUnlessGoSDKRuntimeAvailable(t *testing.T, cfg gateway.Config) {
	t.Helper()

	switch cfg.Runtime.Type {
	case "docker":
		skipUnlessDockerRuntimeAvailable(t, cfg)
		return
	case "orbstack":
		skipUnlessOrbstackRuntimeAvailable(t, cfg)
		return
	case "applecontainer":
		skipUnlessAppleContainerRuntimeAvailable(t, cfg)
		return
	}
}

func skipUnlessDockerRuntimeAvailable(t *testing.T, cfg gateway.Config) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(
		client.WithHost(cfg.Docker.Host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Skipf("docker runtime is unavailable: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon is unavailable: %v", err)
	}
	imageRef := firstTaggedDockerImage(t, cli, ctx)
	if imageRef == "" {
		t.Skip("no tagged docker images are available as templates")
	}

	envdBinary := dockerIntegrationEnvdBinary(t, cfg, cli, ctx, imageRef)
	if _, err := os.Stat(envdBinary); err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}
}

func skipUnlessOrbstackRuntimeAvailable(t *testing.T, cfg gateway.Config) {
	t.Helper()

	if runtime.GOOS != "darwin" {
		t.Skipf("orbstack runtime is only available on darwin, got %s", runtime.GOOS)
	}
	if _, err := os.Stat(cfg.Orbstack.EnvdBinary); err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}

	baseMachine := orbstackConfiguredBaseMachine(cfg)
	if baseMachine == "" {
		baseMachine = orbstackTestBaseMachine()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := orbctl.NewClient(orbctl.DefaultSconRPCSocketPath()).Info(ctx, baseMachine); err != nil {
		t.Skipf("orbstack base machine %q is unavailable: %v", baseMachine, err)
	}
}

func skipUnlessAppleContainerRuntimeAvailable(t *testing.T, cfg gateway.Config) {
	t.Helper()

	if runtime.GOOS != "darwin" {
		t.Skipf("applecontainer runtime is only available on darwin, got %s", runtime.GOOS)
	}
	info, err := os.Stat(cfg.AppleContainer.EnvdBinary)
	if err != nil {
		t.Skipf("envd binary is unavailable: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Skipf("envd binary is not executable: %s", cfg.AppleContainer.EnvdBinary)
	}
	if appleContainerConfiguredTemplateID(cfg) == "" {
		t.Skip("applecontainer.templates has no configured templates")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runtime, err := gateway.NewSandboxRuntime(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("applecontainer runtime is unavailable: %v", err)
	}
	if closer, ok := runtime.(interface{ Close() }); ok {
		t.Cleanup(closer.Close)
	}
	restorer, ok := runtime.(gateway.SandboxRuntimeRestorer)
	if !ok {
		return
	}
	if _, err := restorer.RestoreSandboxes(ctx); err != nil {
		t.Skipf("apple container apiserver is unavailable: %v", err)
	}
}

func goSDKTemplateID(t *testing.T, cfg gateway.Config) string {
	t.Helper()

	if cfg.Runtime.Type == "docker" {
		return dockerTemplateName(dockerIntegrationImageRef(t, cfg))
	}
	if cfg.Runtime.Type == "orbstack" {
		if templateID := orbstackConfiguredTemplateID(cfg); templateID != "" {
			return templateID
		}
		if baseMachine := orbstackConfiguredBaseMachine(cfg); baseMachine != "" {
			return baseMachine
		}
		return orbstackTestBaseMachine()
	}
	if cfg.Runtime.Type == "applecontainer" {
		if templateID := appleContainerConfiguredTemplateID(cfg); templateID != "" {
			return templateID
		}
	}
	return "base"
}

func goSDKOperationTimeout(cfg gateway.Config, fallback time.Duration) time.Duration {
	if cfg.Runtime.Type == "applecontainer" && fallback < appleContainerSDKTimeout {
		return appleContainerSDKTimeout
	}
	return fallback
}

func dockerIntegrationImageRef(t *testing.T, cfg gateway.Config) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(
		client.WithHost(cfg.Docker.Host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Skipf("docker runtime is unavailable: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon is unavailable: %v", err)
	}
	if imageRef := firstTaggedDockerImage(t, cli, ctx); imageRef != "" {
		return imageRef
	}
	t.Skip("no tagged docker images are available as templates")
	return ""
}

func dockerIntegrationEnvdBinary(t *testing.T, cfg gateway.Config, cli *client.Client, ctx context.Context, imageRef string) string {
	t.Helper()

	if envdBinary := strings.TrimSpace(cfg.Docker.EnvdBinary); envdBinary != "" {
		return envdBinary
	}

	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		t.Skipf("inspect docker image platform: %v", err)
	}

	switch normalizeDockerArchitecture(inspect.Architecture) {
	case "amd64":
		return filepath.Join(repoRoot(t), dockerEnvdBinaryAMD64)
	case "arm64":
		return filepath.Join(repoRoot(t), dockerEnvdBinaryARM64)
	default:
		t.Skipf("docker image architecture %q has no bundled envd binary", inspect.Architecture)
		return ""
	}
}

func normalizeDockerArchitecture(architecture string) string {
	switch strings.TrimSpace(architecture) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.TrimSpace(architecture)
	}
}

func firstTaggedDockerImage(t *testing.T, cli *client.Client, ctx context.Context) string {
	t.Helper()

	images, err := cli.ImageList(ctx, image.ListOptions{})
	if err != nil {
		t.Skipf("list docker images: %v", err)
	}

	refs := []string{}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			tag = strings.TrimSpace(tag)
			if tag == "" || tag == "<none>:<none>" {
				continue
			}
			refs = append(refs, tag)
		}
	}
	sort.Strings(refs)
	if len(refs) == 0 {
		return ""
	}
	return refs[0]
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return repoRootPath()
}

func repoRootPath() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "../.."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "../.."))
}

func orbstackTestBaseMachine() string {
	if value := strings.TrimSpace(os.Getenv("E2B_ORBSTACK_TEST_BASE_MACHINE")); value != "" {
		return value
	}
	return defaultOrbstackTestBaseMachine
}

func orbstackConfiguredBaseMachine(cfg gateway.Config) string {
	if templateID := orbstackConfiguredTemplateID(cfg); templateID != "" {
		return templateID
	}
	templateIDs := make([]string, 0, len(cfg.Orbstack.Templates))
	for templateID := range cfg.Orbstack.Templates {
		templateID = strings.TrimSpace(templateID)
		if templateID != "" {
			templateIDs = append(templateIDs, templateID)
		}
	}
	sort.Strings(templateIDs)
	for _, templateID := range templateIDs {
		template := cfg.Orbstack.Templates[templateID]
		if value := strings.TrimSpace(template.BaseMachine); value != "" {
			return value
		}
	}
	return ""
}

func orbstackConfiguredTemplateID(cfg gateway.Config) string {
	templateIDs := make([]string, 0, len(cfg.Orbstack.Templates))
	for templateID, template := range cfg.Orbstack.Templates {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		baseMachine := strings.TrimSpace(template.BaseMachine)
		if baseMachine != "" && baseMachine != templateID {
			continue
		}
		templateIDs = append(templateIDs, templateID)
	}
	sort.Strings(templateIDs)
	if len(templateIDs) == 0 {
		return ""
	}
	return templateIDs[0]
}

func appleContainerConfiguredTemplateID(cfg gateway.Config) string {
	templateIDs := make([]string, 0, len(cfg.AppleContainer.Templates))
	for templateID := range cfg.AppleContainer.Templates {
		templateID = strings.TrimSpace(templateID)
		if templateID != "" {
			templateIDs = append(templateIDs, templateID)
		}
	}
	sort.Strings(templateIDs)
	if len(templateIDs) == 0 {
		return ""
	}
	return templateIDs[0]
}

func registerOrbstackIntegrationCleanup(t *testing.T, cfg gateway.Config) {
	t.Helper()

	if cfg.Runtime.Type != "orbstack" {
		return
	}
	t.Cleanup(func() {
		cleanupOrbstackIntegrationMachines(t, cfg)
	})
}

func cleanupOrbstackIntegrationMachines(t *testing.T, cfg gateway.Config) {
	t.Helper()

	if strings.TrimSpace(cfg.Runtime.Type) != "orbstack" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := orbctl.NewClient(orbctl.DefaultSconRPCSocketPath())
	machines, err := client.ListMachines(ctx)
	if err != nil {
		t.Logf("list orbstack machines for cleanup: %v", err)
		return
	}

	prefix := strings.TrimSpace(cfg.Orbstack.MachineNamePrefix)
	for _, machine := range machines {
		name := strings.TrimSpace(machine.Name)
		if name == "" {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if err := client.Delete(context.Background(), name); err != nil {
			t.Logf("delete orbstack machine %q during cleanup: %v", name, err)
		}
	}
}

func dockerTemplateName(imageRef string) string {
	name := shortDockerImageName(imageRef)
	if digestIndex := strings.Index(name, "@"); digestIndex >= 0 {
		return name[:digestIndex]
	}
	if tagIndex := strings.LastIndex(name, ":"); tagIndex >= 0 {
		return name[:tagIndex]
	}
	return name
}

func shortDockerImageName(imageRef string) string {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return ""
	}

	lastSlash := strings.LastIndex(imageRef, "/")
	return imageRef[lastSlash+1:]
}
