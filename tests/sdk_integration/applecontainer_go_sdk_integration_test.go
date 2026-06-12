//go:build go_sdk_integration && darwin && cgo

package sdk_integration_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gateway "e2b-local/internal/gateway"
)

const defaultAppleContainerSDKTestImage = "docker.io/library/debian:bookworm-slim"

func TestGoSDKGatewayAppleContainerDirectEnvd(t *testing.T) {
	cfg := appleContainerSDKIntegrationConfig()
	skipUnlessGoSDKRuntimeAvailable(t, cfg)

	runGoSDKSandboxLifecycle(t, cfg, "go-sdk-applecontainer-direct-ok")
}

func appleContainerSDKIntegrationConfig() gateway.Config {
	cfg := gateway.DefaultConfig()
	cfg.Runtime.Type = "applecontainer"
	cfg.AppleContainer.ContainerNamePrefix = fmt.Sprintf("e2b-sdk-apple-%d-", time.Now().UTC().UnixNano())
	cfg.AppleContainer.HealthTimeoutSeconds = 120
	cfg.AppleContainer.Templates = map[string]gateway.AppleContainerTemplateConfig{
		"applecontainer-debian-slim": {
			Image: appleContainerSDKIntegrationImage(),
		},
	}
	return cfg
}

func appleContainerSDKIntegrationImage() string {
	if value := strings.TrimSpace(os.Getenv("E2B_APPLECONTAINER_TEST_IMAGE")); value != "" {
		return value
	}
	return defaultAppleContainerSDKTestImage
}
