package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"testing"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"
)

func TestRunVolumeCreatePrintsCreatedVolume(t *testing.T) {
	restore := stubCLIHelpers(t)
	defer restore()

	cfg := gateway.DefaultConfig()
	runtime := &testVolumeRuntime{
		createdVolume: gateway.RuntimeVolume{
			VolumeID: "vol-1",
			Name:     "data",
		},
	}

	loadGatewayConfig = func(path string) (gateway.Config, error) {
		if path != "custom.yaml" {
			t.Fatalf("expected config path custom.yaml, got %q", path)
		}
		return cfg, nil
	}
	newGatewayApp = func(cfg gateway.Config, logger *log.Logger) (http.Handler, error) {
		return gateway.NewAppWithRuntime(cfg, logger, runtime)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"volume", "create", "--config", "custom.yaml", "data"}, &stdout, &stderr, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("run volume create: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	if len(runtime.createdNames) != 1 || runtime.createdNames[0] != "data" {
		t.Fatalf("expected create volume called with data, got %#v", runtime.createdNames)
	}

	var created e2bapi.VolumeAndToken
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("decode volume create output: %v", err)
	}
	if created.VolumeID != "vol-1" || created.Name != "data" || created.Token != "compat-volume-token-vol-1" {
		t.Fatalf("unexpected volume output: %#v", created)
	}
}

func TestRunDefaultsToServeWithConfigFlag(t *testing.T) {
	restore := stubCLIHelpers(t)
	defer restore()

	cfg := gateway.DefaultConfig()
	cfg.Server.Addr = "127.0.0.1:3999"

	loadGatewayConfig = func(path string) (gateway.Config, error) {
		if path != "root-config.yaml" {
			t.Fatalf("expected config path root-config.yaml, got %q", path)
		}
		return cfg, nil
	}

	newGatewayApp = func(cfg gateway.Config, logger *log.Logger) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil
	}

	var listenedAddr string
	listenAndServe = func(addr string, handler http.Handler) error {
		listenedAddr = addr
		if handler == nil {
			t.Fatal("expected non-nil handler")
		}
		return nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"--config", "root-config.yaml"}, &stdout, &stderr, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("run default serve: %v", err)
	}
	if listenedAddr != "127.0.0.1:3999" {
		t.Fatalf("expected listen addr 127.0.0.1:3999, got %q", listenedAddr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunVolumeCreatePostsVolumeRequest(t *testing.T) {
	restore := stubCLIHelpers(t)
	defer restore()

	cfg := gateway.DefaultConfig()

	loadGatewayConfig = func(path string) (gateway.Config, error) {
		return cfg, nil
	}
	newGatewayApp = func(cfg gateway.Config, logger *log.Logger) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/volumes" {
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			}

			var req e2bapi.NewVolume
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if req.Name != "secured-data" {
				t.Fatalf("expected volume name secured-data, got %q", req.Name)
			}

			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"secured-data","token":"token-vol-2","volumeID":"vol-2"}`)
		}), nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"volume", "create", "secured-data", "--config", "secure.yaml"}, &stdout, &stderr, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("run secured volume create: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}

	var created e2bapi.VolumeAndToken
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("decode stdout: %v", err)
	}
	if created.Token != "token-vol-2" || created.VolumeID != "vol-2" || created.Name != "secured-data" {
		t.Fatalf("unexpected volume output: %#v", created)
	}
}

func TestRunVolumeCreateAcceptsConfigBeforeOrAfterName(t *testing.T) {
	testCases := []struct {
		name           string
		args           []string
		wantConfigPath string
		wantVolumeName string
	}{
		{
			name:           "config before name",
			args:           []string{"volume", "create", "--config", "before.yaml", "data-before"},
			wantConfigPath: "before.yaml",
			wantVolumeName: "data-before",
		},
		{
			name:           "config after name",
			args:           []string{"volume", "create", "data-after", "--config", "after.yaml"},
			wantConfigPath: "after.yaml",
			wantVolumeName: "data-after",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			restore := stubCLIHelpers(t)
			defer restore()

			cfg := gateway.DefaultConfig()
			runtime := &testVolumeRuntime{
				createdVolume: gateway.RuntimeVolume{
					VolumeID: testCase.wantVolumeName + "-id",
					Name:     testCase.wantVolumeName,
				},
			}

			loadGatewayConfig = func(path string) (gateway.Config, error) {
				if path != testCase.wantConfigPath {
					t.Fatalf("expected config path %q, got %q", testCase.wantConfigPath, path)
				}
				return cfg, nil
			}
			newGatewayApp = func(cfg gateway.Config, logger *log.Logger) (http.Handler, error) {
				return gateway.NewAppWithRuntime(cfg, logger, runtime)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := run(context.Background(), testCase.args, &stdout, &stderr, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatalf("run volume create: %v", err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got %q", stderr.String())
			}
			if len(runtime.createdNames) != 1 || runtime.createdNames[0] != testCase.wantVolumeName {
				t.Fatalf("expected create volume called with %q, got %#v", testCase.wantVolumeName, runtime.createdNames)
			}
		})
	}
}

func stubCLIHelpers(t *testing.T) func() {
	t.Helper()

	originalLoadGatewayConfig := loadGatewayConfig
	originalNewGatewayApp := newGatewayApp
	originalListenAndServe := listenAndServe

	return func() {
		loadGatewayConfig = originalLoadGatewayConfig
		newGatewayApp = originalNewGatewayApp
		listenAndServe = originalListenAndServe
	}
}

type testVolumeRuntime struct {
	createdNames  []string
	createdVolume gateway.RuntimeVolume
}

func (r *testVolumeRuntime) CreateSandbox(ctx context.Context, req gateway.SandboxRuntimeCreateRequest) (gateway.SandboxRuntimeInfo, error) {
	return gateway.SandboxRuntimeInfo{}, nil
}

func (r *testVolumeRuntime) ListTemplates(ctx context.Context) ([]gateway.SandboxRuntimeTemplate, error) {
	return nil, nil
}

func (r *testVolumeRuntime) DeleteSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) error {
	return nil
}

func (r *testVolumeRuntime) PauseSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) error {
	return nil
}

func (r *testVolumeRuntime) ResumeSandbox(ctx context.Context, info gateway.SandboxRuntimeInfo) (gateway.SandboxRuntimeInfo, error) {
	return info, nil
}

func (r *testVolumeRuntime) CreateVolume(ctx context.Context, name string) (gateway.RuntimeVolume, error) {
	r.createdNames = append(r.createdNames, name)
	return r.createdVolume, nil
}

func (r *testVolumeRuntime) ListVolumes(ctx context.Context) ([]gateway.RuntimeVolume, error) {
	return nil, nil
}

func (r *testVolumeRuntime) GetVolume(ctx context.Context, volumeID string) (gateway.RuntimeVolume, error) {
	return gateway.RuntimeVolume{}, nil
}

func (r *testVolumeRuntime) DeleteVolume(ctx context.Context, volumeID string) (bool, error) {
	return false, nil
}
