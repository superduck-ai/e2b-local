package sbxbackend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"e2b-local/internal/e2bapi"
	gateway "e2b-local/internal/gateway"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

var (
	_ gateway.SandboxRuntime          = (*SbxRuntime)(nil)
	_ gateway.SandboxRuntimeInspector = (*SbxRuntime)(nil)
	_ gateway.SandboxRuntimeRestorer  = (*SbxRuntime)(nil)
	_ gateway.SandboxRuntimeMetrics   = (*SbxRuntime)(nil)
	_ gateway.SandboxRuntimeLogger    = (*SbxRuntime)(nil)
	_ gateway.VolumeRuntime           = (*SbxRuntime)(nil)
	_ gateway.VolumeContentRuntime    = (*SbxRuntime)(nil)
)

type fakeSandboxdClient struct {
	loggedIn bool
	err      error
}

func (c *fakeSandboxdClient) IsAuthenticated(context.Context) (bool, error) {
	return c.loggedIn, c.err
}

func (c *fakeSandboxdClient) CreateSandbox(context.Context, sandboxdCreateRequest) (sandboxdSandbox, error) {
	return sandboxdSandbox{}, errors.New("unexpected CreateSandbox call")
}

func (c *fakeSandboxdClient) InspectSandbox(context.Context, string) (sandboxdSandbox, error) {
	return sandboxdSandbox{}, errors.New("unexpected InspectSandbox call")
}

func (c *fakeSandboxdClient) StartSandbox(context.Context, string) error {
	return errors.New("unexpected StartSandbox call")
}

func (c *fakeSandboxdClient) StopSandbox(context.Context, string) error {
	return errors.New("unexpected StopSandbox call")
}

func (c *fakeSandboxdClient) DeleteSandbox(context.Context, string) error {
	return errors.New("unexpected DeleteSandbox call")
}

func (c *fakeSandboxdClient) Exec(context.Context, string, []string) (sandboxdExecResult, error) {
	return sandboxdExecResult{}, errors.New("unexpected Exec call")
}

func (c *fakeSandboxdClient) GetFile(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected GetFile call")
}

func TestSbxRequiresAuthenticatedSandboxd(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	runtime := &SbxRuntime{
		cfg:            gateway.DefaultConfig().Sbx,
		sandboxdClient: &fakeSandboxdClient{},
		now:            func() time.Time { return now },
	}

	err := runtime.requireAuthenticated(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sbx login") {
		t.Fatalf("expected login error mentioning sbx login, got %v", err)
	}
}

func TestSbxAcceptsAuthenticatedSandboxd(t *testing.T) {
	runtime := &SbxRuntime{
		cfg:            gateway.DefaultConfig().Sbx,
		sandboxdClient: &fakeSandboxdClient{loggedIn: true},
		now:            time.Now,
	}
	if err := runtime.requireAuthenticated(context.Background()); err != nil {
		t.Fatalf("require authenticated sandboxd: %v", err)
	}
}

func TestSbxRuntimeEnvironmentIncludesRuntimeContract(t *testing.T) {
	runtime := &SbxRuntime{cfg: gateway.SbxRuntimeConfig{SbxRuntimeOverrides: gateway.SbxRuntimeOverrides{EnvdPort: 50123}}}
	createdAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	env, err := runtime.runtimeEnvironment(gateway.SandboxRuntimeCreateRequest{
		SandboxID:  "sbx-test",
		TemplateID: "sbx",
		Metadata:   map[string]string{"source": "unit-test"},
		CreatedAt:  createdAt,
		EndAt:      createdAt.Add(time.Hour),
	}, gateway.SbxTemplateConfig{StartCmd: "echo ready"},
		[]gateway.VolumeMount{{VolumeID: "volume-1", Path: "/data"}},
		[]sbxVolumeSpec{{Source: "/workspace/volume-1", Target: "/data"}},
		[]TunnelSpec{{RelayAddress: "192.0.2.1:40000", Token: "token", TargetPort: 50123, PublicPort: 40000, Connections: 2}},
	)
	if err != nil {
		t.Fatalf("build runtime environment: %v", err)
	}
	if env[sbxManagedEnv] != "true" {
		t.Fatalf("missing managed environment values: %#v", env)
	}
	if env[sbxEnvdPortEnv] != "50123" {
		t.Fatalf("expected configured envd port, got %#v", env)
	}
	if env[sbxStartCommandEnv] != "echo ready" {
		t.Fatalf("expected configured start command, got %#v", env)
	}
	var volumes []gateway.VolumeMount
	if err := json.Unmarshal([]byte(env[sbxVolumeMountsEnv]), &volumes); err != nil {
		t.Fatalf("decode volume environment: %v", err)
	}
	if len(volumes) != 1 || volumes[0].Path != "/data" {
		t.Fatalf("unexpected volume environment: %#v", volumes)
	}
}

func TestSbxRuntimeInfoFromContainerRestoresTunnelPorts(t *testing.T) {
	runtime := &SbxRuntime{cfg: gateway.SbxRuntimeConfig{SbxRuntimeOverrides: gateway.SbxRuntimeOverrides{EnvdPort: 49983, TunnelPublicHost: "127.0.0.1"}}}
	tunnelData, err := json.Marshal([]TunnelSpec{
		{TargetPort: 49983, PublicPort: 40001},
		{TargetPort: 8080, PublicPort: 40002},
	})
	if err != nil {
		t.Fatalf("encode tunnels: %v", err)
	}
	info, err := runtime.runtimeInfoFromContainer(gateway.SandboxRuntimeInfo{}, dockertypes.ContainerJSON{
		ContainerJSONBase: &dockertypes.ContainerJSONBase{ID: "container-id", Name: "/e2b-sbx-test"},
		Config: &container.Config{Env: []string{
			sbxSandboxIDEnv + "=sandbox-id",
			sbxManagedEnv + "=true",
			sbxTunnelsEnv + "=" + string(tunnelData),
		}},
	})
	if err != nil {
		t.Fatalf("restore runtime info: %v", err)
	}
	if info.SandboxID != "sandbox-id" || info.ContainerID != "container-id" || info.ContainerName != "e2b-sbx-test" {
		t.Fatalf("unexpected restored identity: %#v", info)
	}
	if info.EnvdURL != "http://127.0.0.1:40001" || info.HostPort != "40001" {
		t.Fatalf("unexpected envd endpoint: %#v", info)
	}
	if len(info.PublishedPorts) != 1 || info.PublishedPorts[0].ContainerPort != 8080 || info.PublishedPorts[0].HostPort != 40002 {
		t.Fatalf("unexpected published ports: %#v", info.PublishedPorts)
	}
}

func TestSbxLogEntriesFiltersAndOrders(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 1, 0, 0, time.UTC)
	start := now.Add(-time.Minute).UnixMilli()
	needle := "ready"
	direction := e2bapi.LogsDirectionBackward
	entries := sbxLogEntries(strings.Join([]string{
		now.Add(-2*time.Minute).Format(time.RFC3339Nano) + " ignored",
		now.Format(time.RFC3339Nano) + " ready first",
		now.Add(time.Second).Format(time.RFC3339Nano) + " ready second",
	}, "\n"), gateway.SandboxLogsRequest{
		Start:     &start,
		Search:    &needle,
		Direction: &direction,
	}, now)
	if len(entries) != 2 || entries[0].Message != "ready second" || entries[1].Message != "ready first" {
		t.Fatalf("unexpected filtered logs: %#v", entries)
	}
}

func TestSbxMetricsParsesPrometheusSamples(t *testing.T) {
	values, err := parsePrometheusMetrics([]byte("# HELP guest_mem_total_pages pages\nguest_mem_total_pages 1024\nguest_mem_available_pages{source=\"balloon\"} 256\nguest_cpu_user_jiffies_total 100\nguest_cpu_system_jiffies_total 50\n"))
	if err != nil {
		t.Fatalf("parse Prometheus metrics: %v", err)
	}
	if values["guest_mem_total_pages"] != 1024 || values["guest_mem_available_pages"] != 256 {
		t.Fatalf("unexpected parsed memory metrics: %#v", values)
	}
	if got := metricValueBySuffix(values, "guest_cpu_system_jiffies_total"); got != 50 {
		t.Fatalf("expected CPU metric 50, got %v", got)
	}
}

func TestSbxRuntimeStatePersistsBootstrapContractPrivately(t *testing.T) {
	root := t.TempDir()
	runtime := &SbxRuntime{cfg: gateway.SbxRuntimeConfig{VolumeHostPath: root}}
	allowInternet := true
	createdAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	state := newSbxRuntimeState(gateway.SandboxRuntimeCreateRequest{
		SandboxID:           "sandbox-state-test",
		TemplateID:          "sbx",
		Metadata:            map[string]string{"source": "unit-test"},
		EnvVars:             map[string]string{"USER_TOKEN": "super-secret"},
		CreatedAt:           createdAt,
		EndAt:               createdAt.Add(time.Hour),
		AllowInternetAccess: &allowInternet,
	}, gateway.SandboxRuntimeInfo{
		SandboxID:     "sandbox-state-test",
		ContainerID:   "container-id",
		ContainerName: "e2b-sbx-sandbox-state-test",
		MachineID:     "sandboxd-id",
		EnvdURL:       "http://127.0.0.1:40000",
	}, map[string]string{
		"USER_TOKEN":           "super-secret",
		sbxManagedEnv:          "true",
		sbxBootstrapVolumesEnv: "[]",
	}, []TunnelSpec{{RelayAddress: "127.0.0.1:40000", Token: "tunnel-token", TargetPort: 49983, PublicPort: 40000}})

	if err := runtime.saveRuntimeState(state); err != nil {
		t.Fatalf("save runtime state: %v", err)
	}
	path, err := runtime.runtimeStatePath(state.SandboxID)
	if err != nil {
		t.Fatalf("resolve runtime state path: %v", err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat runtime state: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected private state file permissions, got %o", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat runtime state directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("expected private state directory permissions, got %o", directoryInfo.Mode().Perm())
	}

	loaded, found, err := runtime.loadRuntimeState(state.SandboxID)
	if err != nil {
		t.Fatalf("load runtime state: %v", err)
	}
	if !found || loaded.Environment["USER_TOKEN"] != "super-secret" || loaded.RuntimeInfo.MachineID != "sandboxd-id" {
		t.Fatalf("unexpected restored bootstrap contract: %#v", loaded)
	}
	if len(loaded.Tunnels) != 1 || loaded.Tunnels[0].OnError != nil {
		t.Fatalf("unexpected restored tunnel state: %#v", loaded.Tunnels)
	}
	if err := runtime.removeRuntimeState(state.SandboxID); err != nil {
		t.Fatalf("remove runtime state: %v", err)
	}
	if _, found, err := runtime.loadRuntimeState(state.SandboxID); err != nil || found {
		t.Fatalf("expected removed runtime state, found=%t err=%v", found, err)
	}
}

func TestDialLongUnixSocketDoesNotChangeWorkingDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), strings.Repeat("nested-", 10))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	currentDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	shortDir, err := os.MkdirTemp("/tmp", "sbx-metrics-test-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	shortParent := filepath.Join(shortDir, "socket-parent")
	if err := os.Symlink(directory, shortParent); err != nil {
		t.Fatalf("link socket directory: %v", err)
	}
	listener, err := net.Listen("unix", filepath.Join(shortParent, "metrics.sock"))
	if err != nil {
		t.Fatalf("listen on relative metrics socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	connection, err := dialLongUnixSocket(context.Background(), filepath.Join(directory, "metrics.sock"))
	if err != nil {
		t.Fatalf("dial long socket: %v", err)
	}
	defer connection.Close()
	peer := <-accepted
	defer peer.Close()
	afterDial, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory after dial: %v", err)
	}
	if afterDial != currentDirectory {
		t.Fatalf("expected working directory %q after dial, got %q", currentDirectory, afterDial)
	}
}

func TestSbxSandboxdClientUsesUnixHTTPProtocol(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "sbx-socket-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "sandboxd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on sandboxd socket: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != sbxUserAgent {
			t.Errorf("unexpected user agent %q", request.Header.Get("User-Agent"))
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/sandbox":
			_, _ = writer.Write([]byte("[]"))
		case request.Method == http.MethodPost && request.URL.Path == "/sandbox":
			var created sandboxdCreateRequest
			if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
				t.Errorf("decode create request: %v", err)
				return
			}
			if created.Name != "e2b-sbx-test" || created.Agent != "shell" || created.Template != "e2b-local/sbx-envd:test" {
				t.Errorf("unexpected create request: %#v", created)
			}
			if len(created.AdditionalWorkspaces) != 1 || created.AdditionalWorkspaces[0].Dir != "/workspace/extra" || created.AdditionalWorkspaces[0].ReadOnly {
				t.Errorf("unexpected additional workspaces: %#v", created.AdditionalWorkspaces)
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(sandboxdSandbox{ID: "sandboxd-id", Name: created.Name, Status: "running"})
		default:
			http.NotFound(writer, request)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	client := newSandboxdClient(socket)
	loggedIn, err := client.IsAuthenticated(context.Background())
	if err != nil || !loggedIn {
		t.Fatalf("expected authenticated Unix socket client, loggedIn=%t err=%v", loggedIn, err)
	}
	created, err := client.CreateSandbox(context.Background(), sandboxdCreateRequest{
		Agent:                "shell",
		Workspace:            "/tmp",
		Template:             "e2b-local/sbx-envd:test",
		Name:                 "e2b-sbx-test",
		AdditionalWorkspaces: []sandboxdAdditionalWorkspace{{Dir: "/workspace/extra"}},
	})
	if err != nil {
		t.Fatalf("create through sandboxd socket: %v", err)
	}
	if created.ID != "sandboxd-id" || created.Name != "e2b-sbx-test" {
		t.Fatalf("unexpected sandboxd response: %#v", created)
	}
}

func TestSbxTunnelRelayBridgesGuestAndUser(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for target: %v", err)
	}
	defer target.Close()
	go func() {
		connection, acceptErr := target.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		buffer := make([]byte, 64)
		count, readErr := connection.Read(buffer)
		if readErr == nil {
			_, _ = connection.Write([]byte(strings.ToUpper(string(buffer[:count]))))
		}
	}()

	relay, err := NewTunnelRelay("127.0.0.1", 0, "test-token")
	if err != nil {
		t.Fatalf("create relay: %v", err)
	}
	defer relay.Close()
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunTunnel(ctx, TunnelSpec{
			RelayAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.Port())),
			Token:        "test-token",
			TargetPort:   target.Addr().(*net.TCPAddr).Port,
			Connections:  1,
		})
	}()
	defer func() {
		cancel()
		if err := <-workerDone; err != nil {
			t.Errorf("stop tunnel worker: %v", err)
		}
	}()

	user, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.Port())), 5*time.Second)
	if err != nil {
		t.Fatalf("connect user to relay: %v", err)
	}
	defer user.Close()
	_ = user.SetDeadline(time.Now().Add(10 * time.Second))
	message := "tunnel payload"
	if _, err := user.Write([]byte(message)); err != nil {
		t.Fatalf("write user payload: %v", err)
	}
	response := make([]byte, len(message))
	if _, err := io.ReadFull(user, response); err != nil {
		t.Fatalf("read tunneled response: %v", err)
	}
	if string(response) != strings.ToUpper(message) {
		t.Fatalf("unexpected tunneled response %q", response)
	}
}

func TestTunnelRelayCloseInterruptsActiveBridge(t *testing.T) {
	relay, err := NewTunnelRelay("127.0.0.1", 0, "test-token")
	if err != nil {
		t.Fatalf("create relay: %v", err)
	}
	leftRelay, leftPeer := net.Pipe()
	rightRelay, rightPeer := net.Pipe()
	defer leftPeer.Close()
	defer rightPeer.Close()
	if !relay.registerBridgeConnections(leftRelay, rightRelay) {
		t.Fatal("register active bridge on open relay")
	}
	bridgeDone := make(chan struct{})
	go func() {
		bridgeConnections(leftRelay, rightRelay)
		relay.unregisterBridgeConnections(leftRelay, rightRelay)
		close(bridgeDone)
	}()

	closed := make(chan error, 1)
	go func() { closed <- relay.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close relay: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay close waited on an active bridge")
	}
	select {
	case <-bridgeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active bridge was not interrupted by relay close")
	}
}
