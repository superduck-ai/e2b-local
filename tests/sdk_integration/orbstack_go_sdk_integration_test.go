//go:build go_sdk_integration && darwin

package sdk_integration_test

import (
	"fmt"
	"testing"
	"time"

	gateway "e2b-local/internal/gateway"
)

func TestGoSDKGatewayOrbstackDirectEnvd(t *testing.T) {
	cfg := orbstackSDKIntegrationConfig()
	skipUnlessGoSDKRuntimeAvailable(t, cfg)
	registerOrbstackIntegrationCleanup(t, cfg)

	runGoSDKSandboxLifecycle(t, cfg, "go-sdk-orbstack-direct-ok")
}

func orbstackSDKIntegrationConfig() gateway.Config {
	cfg := gateway.DefaultConfig()
	cfg.Runtime.Type = "orbstack"
	cfg.Orbstack.MachineNamePrefix = fmt.Sprintf("e2b-sdk-orb-%d-", time.Now().UTC().UnixNano())
	cfg.Orbstack.HealthTimeoutSeconds = 120
	return cfg
}
