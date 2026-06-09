//go:build js_sdk_integration

package sdk_integration_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	gateway "e2b-local/internal/gateway"
)

func TestJSSDKGatewaySmoke(t *testing.T) {
	sdkImport := jsSDKImportPath(t)
	cfg := goSDKIntegrationConfig(t)
	skipUnlessGoSDKRuntimeAvailable(t, cfg)

	server := httptest.NewServer(gateway.NewApp(cfg, log.New(io.Discard, "", 0)))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", "scripts/js-sdk-smoke.mjs")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(),
		"E2B_API_URL="+server.URL,
		"E2B_SANDBOX_URL=",
		"E2B_API_KEY="+testAPIKey,
		"E2B_DEBUG=false",
		"E2B_JS_SDK_IMPORT="+sdkImport,
	)

	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("JS SDK smoke timed out\n%s", output)
	}
	if err != nil {
		t.Fatalf("run JS SDK smoke: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "sandbox killed") {
		t.Fatalf("expected JS SDK smoke to kill sandbox, got output:\n%s", output)
	}
}

func jsSDKImportPath(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node is unavailable: %v", err)
	}

	if explicit := strings.TrimSpace(os.Getenv("E2B_JS_SDK_IMPORT")); explicit != "" {
		if output, ok := nodeCanImport(explicit); ok {
			return explicit
		} else {
			t.Skipf("E2B_JS_SDK_IMPORT is not importable: %s\n%s", explicit, output)
		}
	}

	if output, ok := nodeCanImport("e2b"); ok {
		return "e2b"
	} else if strings.TrimSpace(output) != "" {
		t.Logf("installed e2b package is not importable:\n%s", output)
	}

	t.Skip("JS SDK is unavailable; install e2b or set E2B_JS_SDK_IMPORT")
	return ""
}

func nodeCanImport(specifier string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	script := fmt.Sprintf("await import(%s)", strconv.Quote(specifier))
	cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script)
	cmd.Dir = repoRootPath()
	output, err := cmd.CombinedOutput()
	return string(output), err == nil
}
