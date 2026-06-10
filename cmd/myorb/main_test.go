package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"e2b-local/internal/orbctl"
)

func TestListJSONFiltersBuiltinAndMatchesOrbShape(t *testing.T) {
	socketPath, closeServer := serveTestUnixRPC(t, func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["method"] != "ListContainers" {
			t.Fatalf("expected ListContainers, got %#v", req)
		}

		_, _ = w.Write([]byte(`{
			"jsonrpc":"2.0",
			"id":1,
			"result":[
				{
					"record":{
						"id":"docker-id",
						"name":"docker",
						"image":{"distro":"docker","version":"latest","arch":"arm64","variant":"default"},
						"config":{"isolated":false,"forward_ssh_agent":true,"isolate_network":false,"default_username":"root","http_port":0,"https_port":0},
						"builtin":true,
						"state":"running"
					},
					"disk_size":null
				},
				{
					"record":{
						"id":"vm-id",
						"name":"ubuntu-2404",
						"image":{"distro":"ubuntu","version":"noble","arch":"arm64","variant":"cloud"},
						"config":{"isolated":false,"forward_ssh_agent":true,"isolate_network":false,"default_username":"arthur","http_port":0,"https_port":0},
						"builtin":false,
						"state":"stopped"
					},
					"disk_size":1024
				}
			]
		}`))
	})
	defer closeServer()

	var stdout bytes.Buffer
	err := run(context.Background(), []string{"--sock", socketPath, "list", "--format", "json"}, &stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("run list: %v", err)
	}

	var records []orbctl.ContainerRecord
	if err := json.Unmarshal(stdout.Bytes(), &records); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if len(records) != 1 || records[0].Name != "ubuntu-2404" {
		t.Fatalf("expected one non-builtin record, got %#v", records)
	}
}

func TestInfoJSONMatchesOrbShape(t *testing.T) {
	socketPath, closeServer := serveTestUnixRPC(t, func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"jsonrpc":"2.0",
			"id":1,
			"result":[{
				"record":{
					"id":"vm-id",
					"name":"ubuntu-2404",
					"image":{"distro":"ubuntu","version":"noble","arch":"arm64","variant":"cloud"},
					"config":{"isolated":false,"forward_ssh_agent":true,"isolate_network":false,"default_username":"arthur","http_port":0,"https_port":0},
					"builtin":false,
					"state":"stopped"
				},
				"disk_size":1024,
				"ip4":"192.168.139.10"
			}]
		}`))
	})
	defer closeServer()

	var stdout bytes.Buffer
	err := run(context.Background(), []string{"--sock", socketPath, "info", "--format", "json", "ubuntu-2404"}, &stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("run info: %v", err)
	}

	var info orbctl.ContainerInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if info.Record.Name != "ubuntu-2404" || info.DiskSize == nil || *info.DiskSize != 1024 || info.IP4 != "192.168.139.10" {
		t.Fatalf("unexpected info output: %#v", info)
	}
}

func TestRunCommandIsExplicitlyUnsupported(t *testing.T) {
	err := run(context.Background(), []string{"run", "--machine", "ubuntu-2404", "true"}, ioDiscard{}, ioDiscard{})
	if err == nil {
		t.Fatal("expected unsupported run error")
	}
	if got := err.Error(); got != "myorb run is not implemented through sconrpc.sock yet" {
		t.Fatalf("unexpected error %q", got)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}

func serveTestUnixRPC(t *testing.T, handler func(*testing.T, http.ResponseWriter, *http.Request)) (string, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "myorb.")
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
