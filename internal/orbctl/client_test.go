package orbctl

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestListContainersOmitsParamsAndDecodesResult(t *testing.T) {
	socketPath, closeServer := serveUnixRPC(t, func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.Host != "sconrpc" {
			t.Fatalf("expected sconrpc host, got %q", r.Host)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("expected json content type, got %q", r.Header.Get("Content-Type"))
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["jsonrpc"] != "2.0" || req["method"] != "ListContainers" {
			t.Fatalf("unexpected request body: %#v", req)
		}
		if _, ok := req["params"]; ok {
			t.Fatalf("ListContainers must omit params, got request body %#v", req)
		}

		_, _ = w.Write([]byte(`{
			"jsonrpc":"2.0",
			"id":1,
			"result":[{
				"record":{
					"id":"01GQQVF6C60000000000DOCKER",
					"name":"docker",
					"image":{
						"distro":"docker",
						"version":"latest",
						"arch":"arm64",
						"variant":"default"
					},
					"config":{
						"isolated":false,
						"forward_ssh_agent":true,
						"isolate_network":false,
						"default_username":"root",
						"http_port":0,
						"https_port":0
					},
					"builtin":true,
					"state":"running"
				},
				"disk_size":null,
				"ip4":"192.168.139.2",
				"ip6":"fd07:b51a:cc66::2"
			}]
		}`))
	})
	defer closeServer()

	containers, err := NewClient(socketPath).ListContainers(context.Background())
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("expected one container, got %#v", containers)
	}

	container := containers[0]
	if container.Record.Name != "docker" || container.Record.Image.Distro != "docker" {
		t.Fatalf("unexpected container record: %#v", container.Record)
	}
	if container.Record.Config.DefaultUsername != "root" || !container.Record.Config.ForwardSSHAgent {
		t.Fatalf("unexpected container config: %#v", container.Record.Config)
	}
	if container.DiskSize != nil {
		t.Fatalf("expected nil disk size, got %#v", container.DiskSize)
	}
	if container.IP4 != "192.168.139.2" || container.IP6 != "fd07:b51a:cc66::2" {
		t.Fatalf("unexpected container ips: %#v", container)
	}
}

func TestListContainersReturnsRPCError(t *testing.T) {
	socketPath, closeServer := serveUnixRPC(t, func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"jsonrpc":"2.0",
			"id":1,
			"error":{"code":-32602,"message":"no parameters accepted"}
		}`))
	})
	defer closeServer()

	_, err := NewClient(socketPath).ListContainers(context.Background())
	if err == nil {
		t.Fatal("expected RPC error")
	}

	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected RPCError, got %T %[1]v", err)
	}
	if rpcErr.Code != -32602 || rpcErr.Message != "no parameters accepted" {
		t.Fatalf("unexpected RPC error: %#v", rpcErr)
	}
}

func TestLifecycleCommandsSendPositionalParams(t *testing.T) {
	tests := []struct {
		name   string
		call   func(context.Context, *Client) error
		method string
		params []any
	}{
		{
			name: "start",
			call: func(ctx context.Context, client *Client) error {
				return client.Start(ctx, "sandbox-1")
			},
			method: "ContainerStart",
			params: []any{"sandbox-1"},
		},
		{
			name: "stop",
			call: func(ctx context.Context, client *Client) error {
				return client.Stop(ctx, "sandbox-1")
			},
			method: "ContainerStop",
			params: []any{"sandbox-1"},
		},
		{
			name: "delete",
			call: func(ctx context.Context, client *Client) error {
				return client.Delete(ctx, "sandbox-1")
			},
			method: "ContainerDelete",
			params: []any{"sandbox-1"},
		},
		{
			name: "clone",
			call: func(ctx context.Context, client *Client) error {
				return client.Clone(ctx, "template-1", "sandbox-1")
			},
			method: "ContainerClone",
			params: []any{"template-1", "sandbox-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socketPath, closeServer := serveUnixRPC(t, func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if req["method"] != tt.method {
					t.Fatalf("expected method %q, got request body %#v", tt.method, req)
				}
				gotParams, ok := req["params"].([]any)
				if !ok {
					t.Fatalf("expected positional params, got request body %#v", req)
				}
				if len(gotParams) != len(tt.params) {
					t.Fatalf("expected %d params, got %#v", len(tt.params), gotParams)
				}
				for i, want := range tt.params {
					if gotParams[i] != want {
						t.Fatalf("param %d: expected %#v, got %#v", i, want, gotParams[i])
					}
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
			})
			defer closeServer()

			if err := tt.call(context.Background(), NewClient(socketPath)); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
		})
	}
}

func TestSetIsolatedReadsExistingConfigAndWritesFullConfig(t *testing.T) {
	requests := 0
	socketPath, closeServer := serveUnixRPC(t, func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		requests++
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		switch requests {
		case 1:
			if req["method"] != "ListContainers" {
				t.Fatalf("expected ListContainers, got %#v", req)
			}
			_, _ = w.Write([]byte(`{
				"jsonrpc":"2.0",
				"id":1,
				"result":[{
					"record":{
						"id":"vm-1",
						"name":"sandbox-1",
						"image":{"distro":"ubuntu","version":"noble","arch":"arm64","variant":"cloud"},
						"config":{
							"isolated":false,
							"forward_ssh_agent":true,
							"isolate_network":false,
							"default_username":"arthur",
							"http_port":0,
							"https_port":0,
							"mounts":[{"source":"/tmp/host","destination":"/mnt/host"}]
						},
						"state":"stopped"
					},
					"disk_size":1024
				}]
			}`))
		case 2:
			if req["method"] != "ContainerSetConfig" {
				t.Fatalf("expected ContainerSetConfig, got %#v", req)
			}
			params, ok := req["params"].([]any)
			if !ok || len(params) != 2 || params[0] != "sandbox-1" {
				t.Fatalf("unexpected params: %#v", req["params"])
			}
			config, ok := params[1].(map[string]any)
			if !ok {
				t.Fatalf("expected config map, got %#v", params[1])
			}
			if config["isolated"] != true || config["forward_ssh_agent"] != true || config["default_username"] != "arthur" {
				t.Fatalf("config was not preserved and updated: %#v", config)
			}
			mounts, ok := config["mounts"].([]any)
			if !ok || len(mounts) != 1 {
				t.Fatalf("expected preserved mounts, got %#v", config["mounts"])
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":null}`))
		default:
			t.Fatalf("unexpected extra request %#v", req)
		}
	})
	defer closeServer()

	if err := NewClient(socketPath).SetIsolated(context.Background(), "sandbox-1", true); err != nil {
		t.Fatalf("set isolated: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
}

func TestDefaultSconRPCSocketPathUsesHome(t *testing.T) {
	oldHome := os.Getenv("HOME")
	t.Cleanup(func() {
		if oldHome == "" {
			_ = os.Unsetenv("HOME")
			return
		}
		_ = os.Setenv("HOME", oldHome)
	})

	_ = os.Setenv("HOME", "/tmp/orbstack-home")
	want := filepath.Join("/tmp/orbstack-home", ".orbstack", "run", "sconrpc.sock")
	if got := DefaultSconRPCSocketPath(); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func serveUnixRPC(t *testing.T, handler func(*testing.T, http.ResponseWriter, *http.Request)) (string, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "orbctl.")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	socketPath := filepath.Join(dir, "sconrpc.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler(t, w, r)
		}),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve unix socket: %v", err)
		}
	}()

	return socketPath, func() {
		_ = server.Close()
		<-done
	}
}
