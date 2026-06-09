package orbstackbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
)

var ErrVMNotFound = errors.New("orbstack vm not found")

type machineClient interface {
	DeleteVM(ctx context.Context, name string) error
	StartVM(ctx context.Context, name string) error
	StopVM(ctx context.Context, name string) error
	GetVMInfo(ctx context.Context, name string) (VMInfo, error)
	ListVMs(ctx context.Context) ([]VMInfo, error)
	CloneVM(ctx context.Context, source string, dest string) error
	AddMachineMount(ctx context.Context, machine string, source string, dest string) error
	PushFile(ctx context.Context, machine string, source string, dest string) error
	SetMachineOption(ctx context.Context, machine string, option string, value string) error
	RunCommand(ctx context.Context, machine string, cmd []string) ([]byte, error)
}

type vmClientRunner func(ctx context.Context, name string, args ...string) ([]byte, error)
type vmClientStreamRunner func(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error)

type VMClient struct {
	orbBinary    string
	logger       *log.Logger
	runner       vmClientRunner
	streamRunner vmClientStreamRunner
}

type CreateVMRequest struct {
	Name     string
	Distro   string
	Version  string
	Arch     string
	Memory   string
	CPUs     string
	Disk     string
	UserData string
	Isolated bool
}

type VMInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	State    string   `json:"state"`
	Image    VMImage  `json:"image"`
	Config   VMConfig `json:"config"`
	Builtin  bool     `json:"builtin"`
	DiskSize int64    `json:"disk_size"`
	IP4      string   `json:"ip4"`
	IP6      string   `json:"ip6"`
}

type VMImage struct {
	Distro  string `json:"distro"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Variant string `json:"variant"`
}

type VMConfig struct {
	Isolated        bool   `json:"isolated"`
	ForwardSSHAgent bool   `json:"forward_ssh_agent"`
	IsolateNetwork  bool   `json:"isolate_network"`
	DefaultUsername string `json:"default_username"`
	HTTPPort        int    `json:"http_port"`
	HTTPSPort       int    `json:"https_port"`
}

func NewVMClient(orbBinary string, logger *log.Logger) *VMClient {
	if strings.TrimSpace(orbBinary) == "" {
		orbBinary = "/usr/local/bin/orb"
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	return &VMClient{
		orbBinary:    orbBinary,
		logger:       logger,
		runner:       execVMClientRunner,
		streamRunner: execVMClientStreamRunner,
	}
}

func (c *VMClient) CreateVM(ctx context.Context, req CreateVMRequest) error {
	distro := strings.TrimSpace(req.Distro)
	if distro == "" {
		return fmt.Errorf("distro is required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fmt.Errorf("name is required")
	}

	args := []string{"create"}
	if value := strings.TrimSpace(req.Arch); value != "" {
		args = append(args, "--arch", value)
	}
	if value := strings.TrimSpace(req.Memory); value != "" {
		args = append(args, "--memory", value)
	}
	if value := strings.TrimSpace(req.CPUs); value != "" {
		args = append(args, "--cpus", value)
	}
	if value := strings.TrimSpace(req.Disk); value != "" {
		args = append(args, "--disk", value)
	}
	if value := strings.TrimSpace(req.UserData); value != "" {
		args = append(args, "--user-data", value)
	}
	if req.Isolated {
		args = append(args, "--isolated")
	}

	imageRef := distro
	if version := strings.TrimSpace(req.Version); version != "" {
		imageRef += ":" + version
	}
	args = append(args, imageRef, name)

	_, err := c.run(ctx, args...)
	return err
}

func (c *VMClient) DeleteVM(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if _, err := c.run(ctx, "delete", "--force", name); err != nil {
		if errors.Is(err, ErrVMNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (c *VMClient) StartVM(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	_, err := c.run(ctx, "start", name)
	return err
}

func (c *VMClient) StopVM(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	_, err := c.run(ctx, "stop", name)
	return err
}

func (c *VMClient) GetVMInfo(ctx context.Context, name string) (VMInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return VMInfo{}, fmt.Errorf("name is required")
	}

	output, err := c.run(ctx, "info", "--format", "json", name)
	if err != nil {
		return VMInfo{}, err
	}

	var payload struct {
		Record   VMInfo `json:"record"`
		DiskSize int64  `json:"disk_size"`
		IP4      string `json:"ip4"`
		IP6      string `json:"ip6"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		return VMInfo{}, fmt.Errorf("decode orb info output: %w", err)
	}
	payload.Record.DiskSize = payload.DiskSize
	payload.Record.IP4 = strings.TrimSpace(payload.IP4)
	payload.Record.IP6 = strings.TrimSpace(payload.IP6)
	return payload.Record, nil
}

func (c *VMClient) ListVMs(ctx context.Context) ([]VMInfo, error) {
	output, err := c.run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}

	var vms []VMInfo
	if err := json.Unmarshal(output, &vms); err != nil {
		return nil, fmt.Errorf("decode orb list output: %w", err)
	}
	return vms, nil
}

func (c *VMClient) CloneVM(ctx context.Context, source string, dest string) error {
	source = strings.TrimSpace(source)
	dest = strings.TrimSpace(dest)
	if source == "" || dest == "" {
		return fmt.Errorf("source and dest are required")
	}
	_, err := c.run(ctx, "clone", source, dest)
	return err
}

func (c *VMClient) AddMachineMount(ctx context.Context, machine string, source string, dest string) error {
	machine = strings.TrimSpace(machine)
	source = strings.TrimSpace(source)
	dest = strings.TrimSpace(dest)
	if machine == "" {
		return fmt.Errorf("machine is required")
	}
	if source == "" {
		return fmt.Errorf("source is required")
	}

	spec := source
	if dest != "" {
		spec += ":" + dest
	}
	_, err := c.run(ctx, "config", "add", "machine."+machine+".mounts", spec)
	return err
}

func (c *VMClient) PushFile(ctx context.Context, machine string, source string, dest string) error {
	machine = strings.TrimSpace(machine)
	source = strings.TrimSpace(source)
	dest = strings.TrimSpace(dest)
	if machine == "" {
		return fmt.Errorf("machine is required")
	}
	if source == "" {
		return fmt.Errorf("source is required")
	}
	if dest == "" {
		return fmt.Errorf("dest is required")
	}

	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()

	parent := path.Dir(dest)
	if parent == "." || parent == "" {
		parent = "/"
	}
	script := "set -eu; mkdir -p " + shQuote(parent) + "; cat > " + shQuote(dest)
	_, err = c.runWithStdin(ctx, file, "run", "--machine", machine, "/bin/sh", "-lc", script)
	return err
}

func (c *VMClient) SetMachineOption(ctx context.Context, machine string, option string, value string) error {
	machine = strings.TrimSpace(machine)
	option = strings.TrimSpace(option)
	if machine == "" {
		return fmt.Errorf("machine is required")
	}
	if option == "" {
		return fmt.Errorf("option is required")
	}
	_, err := c.run(ctx, "config", "set", "machine."+machine+"."+option, value)
	return err
}

func (c *VMClient) RunCommand(ctx context.Context, machine string, cmd []string) ([]byte, error) {
	machine = strings.TrimSpace(machine)
	if machine == "" {
		return nil, fmt.Errorf("machine is required")
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("command is required")
	}

	args := []string{"run", "--machine", machine}
	args = append(args, cmd...)
	return c.run(ctx, args...)
}

func (c *VMClient) run(ctx context.Context, args ...string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("vm client is nil")
	}
	if c.runner == nil {
		return nil, fmt.Errorf("vm client runner is nil")
	}

	output, err := c.runner(ctx, c.orbBinary, args...)
	if err == nil {
		return output, nil
	}

	trimmed := strings.TrimSpace(string(output))
	if isVMNotFoundOutput(trimmed) {
		if trimmed == "" {
			trimmed = strings.Join(args, " ")
		}
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, trimmed)
	}
	if trimmed != "" {
		return nil, fmt.Errorf("run %s: %w: %s", strings.Join(args, " "), err, trimmed)
	}
	return nil, fmt.Errorf("run %s: %w", strings.Join(args, " "), err)
}

func (c *VMClient) runWithStdin(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("vm client is nil")
	}
	if c.streamRunner == nil {
		return nil, fmt.Errorf("vm client stream runner is nil")
	}

	output, err := c.streamRunner(ctx, stdin, c.orbBinary, args...)
	if err == nil {
		return output, nil
	}

	trimmed := strings.TrimSpace(string(output))
	if isVMNotFoundOutput(trimmed) {
		if trimmed == "" {
			trimmed = strings.Join(args, " ")
		}
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, trimmed)
	}
	if trimmed != "" {
		return nil, fmt.Errorf("run %s: %w: %s", strings.Join(args, " "), err, trimmed)
	}
	return nil, fmt.Errorf("run %s: %w", strings.Join(args, " "), err)
}

func execVMClientRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func execVMClientStreamRunner(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	return cmd.CombinedOutput()
}

func isVMNotFoundOutput(output string) bool {
	output = strings.ToLower(strings.TrimSpace(output))
	if output == "" {
		return false
	}

	patterns := []string{
		"not found",
		"no machine",
		"unknown machine",
		"does not exist",
	}
	for _, pattern := range patterns {
		if strings.Contains(output, pattern) {
			return true
		}
	}
	return false
}
