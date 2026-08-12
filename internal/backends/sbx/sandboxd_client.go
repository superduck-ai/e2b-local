package sbxbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const sbxUserAgent = "e2b-local/sbx"

type sandboxdAPI interface {
	IsAuthenticated(ctx context.Context) (bool, error)
	CreateSandbox(ctx context.Context, req sandboxdCreateRequest) (sandboxdSandbox, error)
	InspectSandbox(ctx context.Context, name string) (sandboxdSandbox, error)
	StartSandbox(ctx context.Context, name string) error
	StopSandbox(ctx context.Context, name string) error
	DeleteSandbox(ctx context.Context, name string) error
	Exec(ctx context.Context, name string, command []string) (sandboxdExecResult, error)
	GetFile(ctx context.Context, name, path string) (io.ReadCloser, error)
}

type sandboxdClient struct {
	httpClient *http.Client
}

type sandboxdHTTPError struct {
	StatusCode int
	Message    string
}

func (e *sandboxdHTTPError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("sandboxd returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("sandboxd returned HTTP %d: %s", e.StatusCode, e.Message)
}

type sandboxdCreateRequest struct {
	Agent                string                        `json:"agent"`
	Workspace            string                        `json:"workspace"`
	Template             string                        `json:"template,omitempty"`
	Name                 string                        `json:"name,omitempty"`
	Memory               string                        `json:"memory,omitempty"`
	CPUs                 int                           `json:"cpus,omitempty"`
	AdditionalWorkspaces []sandboxdAdditionalWorkspace `json:"additional_workspaces,omitempty"`
	Env                  map[string]string             `json:"env,omitempty"`
}

// sandboxd mounts the primary workspace directly and accepts extra host
// directories only through additional_workspaces, not a []string field.
type sandboxdAdditionalWorkspace struct {
	Dir      string `json:"dir"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type sandboxdSandbox struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Agent     string            `json:"agent"`
	Workspace string            `json:"workspace"`
	Labels    map[string]string `json:"labels"`
}

type sandboxdExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func newSandboxdClient(socketPath string) *sandboxdClient {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &sandboxdClient{httpClient: &http.Client{Transport: transport}}
}

func (c *sandboxdClient) IsAuthenticated(ctx context.Context) (bool, error) {
	_, err := c.listSandboxes(ctx)
	if sandboxdHasStatus(err, http.StatusUnauthorized) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *sandboxdClient) CreateSandbox(ctx context.Context, req sandboxdCreateRequest) (sandboxdSandbox, error) {
	var sandbox sandboxdSandbox
	if err := c.doJSON(ctx, http.MethodPost, "/sandbox", req, &sandbox); err != nil {
		return sandboxdSandbox{}, err
	}
	return sandbox, nil
}

func (c *sandboxdClient) InspectSandbox(ctx context.Context, name string) (sandboxdSandbox, error) {
	var sandbox sandboxdSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(name), nil, &sandbox); err != nil {
		return sandboxdSandbox{}, err
	}
	return sandbox, nil
}

func (c *sandboxdClient) StartSandbox(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(name)+"/start", nil, nil)
}

func (c *sandboxdClient) StopSandbox(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(name)+"/stop", nil, nil)
}

func (c *sandboxdClient) DeleteSandbox(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/sandbox/"+url.PathEscape(name), nil, nil)
}

func (c *sandboxdClient) Exec(ctx context.Context, name string, command []string) (sandboxdExecResult, error) {
	var result sandboxdExecResult
	if err := c.doJSON(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(name)+"/exec", map[string][]string{"cmd": command}, &result); err != nil {
		return sandboxdExecResult{}, err
	}
	return result, nil
}

func (c *sandboxdClient) GetFile(ctx context.Context, name, path string) (io.ReadCloser, error) {
	values := url.Values{}
	values.Set("path", path)
	response, err := c.request(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(name)+"/files?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}

func (c *sandboxdClient) listSandboxes(ctx context.Context) ([]sandboxdSandbox, error) {
	response, err := c.request(ctx, http.MethodGet, "/sandbox", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read sandboxd sandbox list: %w", err)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return []sandboxdSandbox{}, nil
	}
	if bytes.HasPrefix(body, []byte("[")) {
		var sandboxes []sandboxdSandbox
		if err := json.Unmarshal(body, &sandboxes); err != nil {
			return nil, fmt.Errorf("decode sandboxd sandbox list: %w", err)
		}
		return sandboxes, nil
	}
	var result struct {
		Sandboxes []sandboxdSandbox `json:"sandboxes"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode sandboxd sandbox list: %w", err)
	}
	return result.Sandboxes, nil
}

func (c *sandboxdClient) doJSON(ctx context.Context, method, requestPath string, input, output any) error {
	response, err := c.request(ctx, method, requestPath, input)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode sandboxd response: %w", err)
	}
	return nil
}

func (c *sandboxdClient) request(ctx context.Context, method, requestPath string, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode sandboxd request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, "http://sandboxd"+requestPath, body)
	if err != nil {
		return nil, fmt.Errorf("create sandboxd request: %w", err)
	}
	request.Header.Set("User-Agent", sbxUserAgent)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call sandboxd %s %s: %w", method, requestPath, err)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, nil
	}

	defer response.Body.Close()
	message, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	return nil, &sandboxdHTTPError{StatusCode: response.StatusCode, Message: strings.TrimSpace(string(message))}
}

func sandboxdHasStatus(err error, status int) bool {
	var statusError *sandboxdHTTPError
	return errors.As(err, &statusError) && statusError.StatusCode == status
}
