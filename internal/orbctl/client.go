package orbctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const defaultRequestTimeout = 15 * time.Second

var ErrContainerNotFound = errors.New("orbstack container not found")

// DefaultSconRPCSocketPath returns OrbStack's default sconrpc Unix socket path.
func DefaultSconRPCSocketPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		if value, err := os.UserHomeDir(); err == nil {
			home = value
		}
	}
	if home == "" {
		return filepath.Join(".orbstack", "run", "sconrpc.sock")
	}
	return filepath.Join(home, ".orbstack", "run", "sconrpc.sock")
}

// ListContainers calls OrbStack's default sconrpc socket and returns the known
// container records. It mirrors the no-argument orbctl ListContainers RPC.
func ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	return NewClient(DefaultSconRPCSocketPath()).ListContainers(ctx)
}

type Client struct {
	socketPath string
	httpClient *http.Client
	nextID     atomic.Int64
}

type ClientOption func(*Client)

func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			c.httpClient.Timeout = timeout
		}
	}
}

func NewClient(socketPath string, opts ...ClientOption) *Client {
	if socketPath == "" {
		socketPath = DefaultSconRPCSocketPath()
	}

	client := &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Timeout:   defaultRequestTimeout,
			Transport: unixSocketTransport(socketPath),
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client)
		}
	}
	return client
}

func (c *Client) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	var containers []ContainerInfo
	if err := c.call(ctx, "ListContainers", nil, &containers); err != nil {
		return nil, err
	}
	return containers, nil
}

func (c *Client) ListMachines(ctx context.Context) ([]ContainerRecord, error) {
	containers, err := c.ListContainers(ctx)
	if err != nil {
		return nil, err
	}

	machines := make([]ContainerRecord, 0, len(containers))
	for _, container := range containers {
		if container.Record.Builtin {
			continue
		}
		machines = append(machines, container.Record)
	}
	return machines, nil
}

func (c *Client) Info(ctx context.Context, name string) (ContainerInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ContainerInfo{}, fmt.Errorf("name is required")
	}

	containers, err := c.ListContainers(ctx)
	if err != nil {
		return ContainerInfo{}, err
	}
	for _, container := range containers {
		if container.Record.Name == name || container.Record.ID == name {
			return container, nil
		}
	}
	return ContainerInfo{}, fmt.Errorf("%w: %s", ErrContainerNotFound, name)
}

func (c *Client) Start(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return c.call(ctx, "ContainerStart", []any{name}, nil)
}

func (c *Client) Stop(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return c.call(ctx, "ContainerStop", []any{name}, nil)
}

func (c *Client) Delete(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return c.call(ctx, "ContainerDelete", []any{name}, nil)
}

func (c *Client) Clone(ctx context.Context, source string, dest string) error {
	source = strings.TrimSpace(source)
	dest = strings.TrimSpace(dest)
	if source == "" || dest == "" {
		return fmt.Errorf("source and dest are required")
	}
	return c.call(ctx, "ContainerClone", []any{source, dest}, nil)
}

func (c *Client) SetConfig(ctx context.Context, name string, config ContainerConfig) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return c.call(ctx, "ContainerSetConfig", []any{name, config}, nil)
}

func (c *Client) SetIsolated(ctx context.Context, name string, isolated bool) error {
	info, err := c.Info(ctx, name)
	if err != nil {
		return err
	}
	config := info.Record.Config
	config.Isolated = isolated
	return c.SetConfig(ctx, info.Record.Name, config)
}

func (c *Client) AddMount(ctx context.Context, name string, source string, dest string) error {
	source = strings.TrimSpace(source)
	dest = strings.TrimSpace(dest)
	if source == "" {
		return fmt.Errorf("source is required")
	}

	info, err := c.Info(ctx, name)
	if err != nil {
		return err
	}
	config := info.Record.Config
	config.Mounts = append(config.Mounts, MachineMount{
		Source:      source,
		Destination: dest,
	})
	return c.SetConfig(ctx, info.Record.Name, config)
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	if c == nil {
		return fmt.Errorf("orbctl client is nil")
	}
	if c.httpClient == nil {
		return fmt.Errorf("orbctl http client is nil")
	}

	id := c.nextID.Add(1)
	body, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("encode jsonrpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sconrpc", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create jsonrpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("call %s: unexpected HTTP status %d: %s", method, resp.StatusCode, string(respBody))
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return fmt.Errorf("decode %s jsonrpc response: %w", method, err)
	}
	if rpcResp.JSONRPC != "2.0" {
		return fmt.Errorf("decode %s jsonrpc response: unexpected jsonrpc version %q", method, rpcResp.JSONRPC)
	}
	if rpcResp.ID != id {
		return fmt.Errorf("decode %s jsonrpc response: unexpected id %d", method, rpcResp.ID)
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}
	if result == nil {
		return nil
	}
	if len(rpcResp.Result) == 0 {
		return fmt.Errorf("decode %s jsonrpc response: missing result", method)
	}
	if err := json.Unmarshal(rpcResp.Result, result); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

func unixSocketTransport(socketPath string) *http.Transport {
	dialer := net.Dialer{}
	return &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("jsonrpc error %d", e.Code)
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}
