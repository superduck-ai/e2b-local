//go:build darwin && cgo

package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestXPCBridgeUsesAppleRouteAndErrorKeys(t *testing.T) {
	header, err := os.ReadFile("xpc_bridge.h")
	if err != nil {
		t.Fatalf("read xpc_bridge.h: %v", err)
	}
	content := string(header)
	assertStringContains(t, content, `#define XPC_ROUTE_KEY "com.apple.container.xpc.route"`)
	assertStringContains(t, content, `#define XPC_ERROR_KEY "com.apple.container.xpc.error"`)
	if strings.Contains(content, `#define XPC_ROUTE_KEY "route"`) || strings.Contains(content, `#define XPC_ERROR_KEY "error"`) {
		t.Fatalf("bridge must not use plain route/error keys:\n%s", content)
	}
}

func TestXPCBridgeSourceKeepsAsyncReplyAndDefensiveChecks(t *testing.T) {
	source, err := os.ReadFile("xpc_bridge.c")
	if err != nil {
		t.Fatalf("read xpc_bridge.c: %v", err)
	}
	content := string(source)
	assertStringContains(t, content, "xpc_connection_send_message_with_reply(")
	if strings.Contains(content, "xpc_connection_send_message_with_reply_sync") {
		t.Fatalf("bridge must use async reply plus timeout, not sync send")
	}
	assertStringContains(t, content, "pthread_mutex_init(&state->mutex, NULL) != 0")
	assertStringContains(t, content, "pthread_cond_init(&state->cond, NULL) != 0")
	assertStringContains(t, content, "type != XPC_TYPE_DICTIONARY")
	assertStringContains(t, content, "data == NULL && len > 0")
}

func TestProtocolJSONFieldNames(t *testing.T) {
	stopData, err := json.Marshal(containerStopOptions{TimeoutInSeconds: 5})
	if err != nil {
		t.Fatalf("marshal stop options: %v", err)
	}
	assertStringContains(t, string(stopData), `"timeoutInSeconds":5`)
	if strings.Contains(string(stopData), "timeoutSeconds") {
		t.Fatalf("stop options must not encode timeoutSeconds: %s", stopData)
	}

	listData, err := json.Marshal(ContainerListFilters{})
	if err != nil {
		t.Fatalf("marshal list filters: %v", err)
	}
	assertStringContains(t, string(listData), `"ids":[]`)
	assertStringContains(t, string(listData), `"labels":{}`)
	if strings.Contains(string(listData), "all") {
		t.Fatalf("list filters must not encode all:true: %s", listData)
	}

	createOptions, err := json.Marshal(containerCreateOptions{AutoRemove: false})
	if err != nil {
		t.Fatalf("marshal create options: %v", err)
	}
	assertStringContains(t, string(createOptions), `"autoRemove":false`)

	dnsData, err := json.Marshal(dnsConfiguration(nil))
	if err != nil {
		t.Fatalf("marshal DNS configuration: %v", err)
	}
	assertStringContains(t, string(dnsData), `"nameservers":["1.1.1.1"]`)
	assertStringContains(t, string(dnsData), `"searchDomains":[]`)
	assertStringContains(t, string(dnsData), `"options":[]`)

	processData, err := json.Marshal(ProcessConfiguration{
		Executable:         "/bin/sleep",
		Arguments:          []string{appleInitSleepTime},
		Environment:        processEnvironment(nil),
		WorkingDirectory:   "/",
		User:               ProcessUserRoot(),
		SupplementalGroups: []uint32{},
		Rlimits:            []any{},
	})
	if err != nil {
		t.Fatalf("marshal process configuration: %v", err)
	}
	assertStringContains(t, string(processData), `"supplementalGroups":[]`)
	assertStringContains(t, string(processData), `"rlimits":[]`)
}

func TestXPCProtocolErrorDecodesStructuredAppleError(t *testing.T) {
	protocolErr, err := decodeXPCProtocolError([]byte(`{"code":"not_found","message":"container with ID missing not found"}`))
	if err != nil {
		t.Fatalf("decode protocol error: %v", err)
	}
	if protocolErr.Code != appleErrorCodeNotFound || !isAppleNotFound(protocolErr) {
		t.Fatalf("expected not-found protocol error, got %#v", protocolErr)
	}

	wrapped := errors.Join(errors.New("outer"), protocolErr)
	if !isAppleNotFound(wrapped) {
		t.Fatalf("expected wrapped protocol error to be recognized as not found")
	}

	volumeErr := &xpcProtocolError{Code: appleErrorCodeInvalidArg, Message: "volume 'e2b-vol-missing' not found"}
	if !isAppleNotFound(volumeErr) {
		t.Fatalf("expected Apple VolumeError shape to be recognized as not found")
	}
}

func TestXPCPayloadKeyConstantsMatchAppleSource(t *testing.T) {
	tests := map[string]string{
		"container config":    keyContainerConfig,
		"container options":   keyContainerOptions,
		"copy source path":    keySourcePath,
		"copy destination":    keyDestinationPath,
		"copy file mode":      keyFileMode,
		"copy create parents": keyCreateParents,
		"dynamic env":         keyDynamicEnv,
		"image descriptions":  keyImageDescriptions,
		"list filters":        keyListFilters,
		"process config":      keyProcessConfig,
		"stop options":        keyStopOptions,
		"volume labels":       keyVolumeLabels,
	}
	want := map[string]string{
		"container config":    "containerConfig",
		"container options":   "containerOptions",
		"copy source path":    "sourcePath",
		"copy destination":    "destinationPath",
		"copy file mode":      "fileMode",
		"copy create parents": "createParents",
		"dynamic env":         "dynamicEnv",
		"image descriptions":  "imageDescriptions",
		"list filters":        "listFilters",
		"process config":      "processConfig",
		"stop options":        "stopOptions",
		"volume labels":       "volumeLabels",
	}
	for name, got := range tests {
		if got != want[name] {
			t.Fatalf("%s key mismatch: got %q want %q", name, got, want[name])
		}
	}
}

func TestSystemPlatformPayloadDefaultsToLinuxHostArchitecture(t *testing.T) {
	platform := defaultSystemPlatform()
	if platform.OS != "linux" {
		t.Fatalf("expected Linux guest platform, got %#v", platform)
	}
	if platform.Architecture != "arm64" && platform.Architecture != "amd64" {
		t.Fatalf("unexpected platform architecture: %#v", platform)
	}
	data, err := json.Marshal(platform)
	if err != nil {
		t.Fatalf("marshal system platform: %v", err)
	}
	assertStringContains(t, string(data), `"os":"linux"`)
	assertStringContains(t, string(data), `"architecture":"`+platform.Architecture+`"`)
}

func TestTimeoutMillisecondsUsesAtLeastOneMillisecondForPositiveDeadlines(t *testing.T) {
	if got := timeoutMilliseconds(context.Background(), 500*time.Microsecond); got != 1 {
		t.Fatalf("expected sub-millisecond positive timeout to round up to 1ms, got %d", got)
	}
}

func TestImageReferenceMatchingHandlesDockerNormalization(t *testing.T) {
	tests := []struct {
		want string
		got  string
	}{
		{want: "ubuntu", got: "docker.io/library/ubuntu:latest"},
		{want: "ubuntu:24.04", got: "registry-1.docker.io/library/ubuntu:24.04"},
		{want: "docker.io/library/ubuntu:24.04", got: "ubuntu:24.04"},
	}
	for _, test := range tests {
		if !imageReferenceMatches(test.want, test.got) {
			t.Fatalf("expected %q to match %q", test.want, test.got)
		}
	}
	if imageReferenceMatches("alpine:latest", "ubuntu:latest") {
		t.Fatal("different images should not match")
	}
}
