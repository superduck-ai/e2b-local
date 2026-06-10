package orbstackbackend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"e2b-local/internal/orbctl"

	"golang.org/x/crypto/ssh"
)

var ErrVMNotFound = errors.New("orbstack vm not found")

const (
	machineSSHReadyTimeout = 30 * time.Second
	machineSSHRetryDelay   = 500 * time.Millisecond
)

type machineClient interface {
	DeleteVM(ctx context.Context, name string) error
	StartVM(ctx context.Context, name string) error
	StopVM(ctx context.Context, name string) error
	GetVMInfo(ctx context.Context, name string) (VMInfo, error)
	ListVMs(ctx context.Context) ([]VMInfo, error)
	CloneVM(ctx context.Context, source string, dest string) error
	AddMachineMount(ctx context.Context, machine string, source string, dest string) error
	SetMachineOption(ctx context.Context, machine string, option string, value string) error
	MkdirAll(ctx context.Context, machine string, vmPath string, mode fs.FileMode) error
	RemoveAll(ctx context.Context, machine string, vmPath string) error
	ReadFile(ctx context.Context, machine string, vmPath string) ([]byte, error)
	WriteFile(ctx context.Context, machine string, vmPath string, data []byte, mode fs.FileMode) error
	Symlink(ctx context.Context, machine string, oldname string, newname string) error
	RunShell(ctx context.Context, machine string, script string) ([]byte, error)
}

type orbControl interface {
	Delete(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Info(ctx context.Context, name string) (orbctl.ContainerInfo, error)
	ListMachines(ctx context.Context) ([]orbctl.ContainerRecord, error)
	Clone(ctx context.Context, source string, dest string) error
	AddMount(ctx context.Context, name string, source string, dest string) error
	SetIsolated(ctx context.Context, name string, isolated bool) error
}

type VMClient struct {
	orb           orbControl
	orbRoot       string
	sshSocketPath string
	sshKeyPath    string
	sshRunner     machineSSHRunner
	logger        *log.Logger
}

type machineSSHRunner func(ctx context.Context, machine string, stdin io.Reader, script string) ([]byte, error)

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

func NewVMClient(logger *log.Logger) *VMClient {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	return &VMClient{
		orb:           orbctl.NewClient(orbctl.DefaultSconRPCSocketPath()),
		orbRoot:       defaultOrbStackRoot(),
		sshSocketPath: defaultSconSSHSocketPath(),
		sshKeyPath:    defaultOrbStackSSHKeyPath(),
		logger:        logger,
	}
}

func defaultOrbStackRoot() string {
	if value := strings.TrimSpace(os.Getenv("ORBSTACK_ROOT")); value != "" {
		return value
	}
	home := os.Getenv("HOME")
	if home == "" {
		if value, err := os.UserHomeDir(); err == nil {
			home = value
		}
	}
	if home == "" {
		return "OrbStack"
	}
	return filepath.Join(home, "OrbStack")
}

func defaultSconSSHSocketPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		if value, err := os.UserHomeDir(); err == nil {
			home = value
		}
	}
	if home == "" {
		return filepath.Join(".orbstack", "run", "sconssh.sock")
	}
	return filepath.Join(home, ".orbstack", "run", "sconssh.sock")
}

func defaultOrbStackSSHKeyPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		if value, err := os.UserHomeDir(); err == nil {
			home = value
		}
	}
	if home == "" {
		return filepath.Join(".orbstack", "ssh", "id_ed25519")
	}
	return filepath.Join(home, ".orbstack", "ssh", "id_ed25519")
}

func (c *VMClient) CreateVM(ctx context.Context, req CreateVMRequest) error {
	_ = ctx
	_ = req
	return errors.New("orbstack socket client does not support create; clone an existing template machine")
}

func (c *VMClient) DeleteVM(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if err := c.orbClient().Delete(ctx, name); err != nil {
		if isVMNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("delete orbstack vm %s: %w", name, err)
	}
	return nil
}

func (c *VMClient) StartVM(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if err := c.orbClient().Start(ctx, name); err != nil {
		return mapOrbVMError("start", name, err)
	}
	return nil
}

func (c *VMClient) StopVM(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if err := c.orbClient().Stop(ctx, name); err != nil {
		return mapOrbVMError("stop", name, err)
	}
	return nil
}

func (c *VMClient) GetVMInfo(ctx context.Context, name string) (VMInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return VMInfo{}, fmt.Errorf("name is required")
	}

	info, err := c.orbClient().Info(ctx, name)
	if err != nil {
		return VMInfo{}, mapOrbVMError("info", name, err)
	}
	return vmInfoFromContainerInfo(info), nil
}

func (c *VMClient) ListVMs(ctx context.Context) ([]VMInfo, error) {
	records, err := c.orbClient().ListMachines(ctx)
	if err != nil {
		return nil, fmt.Errorf("list orbstack vms: %w", err)
	}

	vms := make([]VMInfo, 0, len(records))
	for _, record := range records {
		vms = append(vms, vmInfoFromContainerRecord(record))
	}
	return vms, nil
}

func (c *VMClient) CloneVM(ctx context.Context, source string, dest string) error {
	source = strings.TrimSpace(source)
	dest = strings.TrimSpace(dest)
	if source == "" || dest == "" {
		return fmt.Errorf("source and dest are required")
	}
	if err := c.orbClient().Clone(ctx, source, dest); err != nil {
		return mapOrbVMError("clone", dest, err)
	}
	return nil
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
	if err := c.orbClient().AddMount(ctx, machine, source, dest); err != nil {
		return mapOrbVMError("config add mount", machine, err)
	}
	return nil
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

	switch option {
	case "isolated":
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("invalid isolated value %q", value)
		}
		if err := c.orbClient().SetIsolated(ctx, machine, parsed); err != nil {
			return mapOrbVMError("config set isolated", machine, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported orbstack machine option %q", option)
	}
}

func (c *VMClient) MkdirAll(ctx context.Context, machine string, vmPath string, mode fs.FileMode) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	vmPath, err := cleanVMPath(vmPath)
	if err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o755
	}
	script := fmt.Sprintf("set -eu\nsudo mkdir -p -- %s\nsudo chmod %04o -- %s\n",
		shQuote(vmPath),
		mode.Perm(),
		shQuote(vmPath),
	)
	if _, err := c.runMachineShell(ctx, machine, nil, script); err != nil {
		return fmt.Errorf("mkdir %s in orbstack vm %s: %w", vmPath, machine, err)
	}
	return nil
}

func (c *VMClient) RemoveAll(ctx context.Context, machine string, vmPath string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	vmPath, err := cleanVMPath(vmPath)
	if err != nil {
		return err
	}
	if vmPath == "/" {
		return fmt.Errorf("refusing to remove vm root for machine %s", machine)
	}
	script := fmt.Sprintf("set -eu\nsudo rm -rf -- %s\n", shQuote(vmPath))
	if _, err := c.runMachineShell(ctx, machine, nil, script); err != nil {
		return fmt.Errorf("remove %s in orbstack vm %s: %w", vmPath, machine, err)
	}
	return nil
}

func (c *VMClient) ReadFile(ctx context.Context, machine string, vmPath string) ([]byte, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	hostPath, err := c.machineFilePath(machine, vmPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(hostPath)
	if err != nil {
		output, sshErr := c.runMachineShell(ctx, machine, nil, fmt.Sprintf("sudo cat -- %s\n", shQuote(path.Clean("/"+strings.TrimPrefix(vmPath, "/")))))
		if sshErr == nil {
			return output, nil
		}
		return nil, fmt.Errorf("read %s in orbstack vm %s: %w", vmPath, machine, err)
	}
	return data, nil
}

func (c *VMClient) WriteFile(ctx context.Context, machine string, vmPath string, data []byte, mode fs.FileMode) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	vmPath, err := cleanVMPath(vmPath)
	if err != nil {
		return err
	}
	parent := path.Dir(vmPath)
	script := fmt.Sprintf(`set -eu
tmp=$(mktemp /tmp/e2b-local.XXXXXX)
cleanup() { rm -f "$tmp"; }
trap cleanup EXIT
cat > "$tmp"
sudo mkdir -p -- %s
sudo install -m %04o "$tmp" %s
`, shQuote(parent), mode.Perm(), shQuote(vmPath))
	if _, err := c.runMachineShell(ctx, machine, bytes.NewReader(data), script); err != nil {
		return fmt.Errorf("install %s in orbstack vm %s: %w", vmPath, machine, err)
	}
	return nil
}

func (c *VMClient) Symlink(ctx context.Context, machine string, oldname string, newname string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	oldname = strings.TrimSpace(oldname)
	if oldname == "" {
		return fmt.Errorf("symlink target is required")
	}
	newname, err := cleanVMPath(newname)
	if err != nil {
		return err
	}
	if newname == "/" {
		return fmt.Errorf("refusing to replace vm root symlink for machine %s", machine)
	}
	script := fmt.Sprintf("set -eu\nsudo mkdir -p -- %s\nsudo rm -rf -- %s\nsudo ln -s -- %s %s\n",
		shQuote(path.Dir(newname)),
		shQuote(newname),
		shQuote(oldname),
		shQuote(newname),
	)
	if _, err := c.runMachineShell(ctx, machine, nil, script); err != nil {
		return fmt.Errorf("create symlink %s -> %s in orbstack vm %s: %w", newname, oldname, machine, err)
	}
	return nil
}

func (c *VMClient) RunShell(ctx context.Context, machine string, script string) ([]byte, error) {
	return c.runMachineShell(ctx, machine, nil, script)
}

func (c *VMClient) runMachineShell(ctx context.Context, machine string, stdin io.Reader, script string) ([]byte, error) {
	machine = strings.TrimSpace(machine)
	if machine == "" {
		return nil, fmt.Errorf("machine is required")
	}
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("script is required")
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if c != nil && c.sshRunner != nil {
		return c.sshRunner(ctx, machine, stdin, script)
	}

	var stdinData []byte
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read command stdin: %w", err)
		}
		stdinData = data
	}
	deadline := time.Now().Add(machineSSHReadyTimeout)
	var lastErr error
	for {
		var attemptStdin io.Reader
		if stdinData != nil {
			attemptStdin = bytes.NewReader(stdinData)
		}
		output, err := c.runMachineSSH(ctx, machine, attemptStdin, script)
		if err == nil {
			return output, nil
		}
		if !isRetryableMachineSSHError(err) || ctxErr(ctx) != nil || time.Now().After(deadline) {
			return nil, err
		}
		lastErr = err

		timer := time.NewTimer(machineSSHRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if lastErr != nil && time.Now().After(deadline) {
			return nil, lastErr
		}
	}
}

func (c *VMClient) runMachineSSH(ctx context.Context, machine string, stdin io.Reader, script string) ([]byte, error) {
	socketPath := defaultSconSSHSocketPath()
	keyPath := defaultOrbStackSSHKeyPath()
	if c != nil {
		if strings.TrimSpace(c.sshSocketPath) != "" {
			socketPath = c.sshSocketPath
		}
		if strings.TrimSpace(c.sshKeyPath) != "" {
			keyPath = c.sshKeyPath
		}
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read orbstack ssh key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse orbstack ssh key %s: %w", keyPath, err)
	}

	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial orbstack ssh socket %s: %w", socketPath, err)
	}
	defer conn.Close()

	sshUser, err := c.machineSSHUser(ctx, machine)
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User: sshUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, "orb", config)
	if err != nil {
		return nil, fmt.Errorf("open orbstack ssh session for %s: %w", machine, err)
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("create orbstack ssh session for %s: %w", machine, err)
	}
	defer session.Close()
	if stdin != nil {
		session.Stdin = stdin
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	command := "/bin/sh -lc " + shQuote(script)
	if err := session.Start(command); err != nil {
		return nil, fmt.Errorf("start orbstack ssh command for %s: %w", machine, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("orbstack ssh command failed for %s: %s", machine, message)
		}
	}
	return stdout.Bytes(), nil
}

func isRetryableMachineSSHError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	patterns := []string{
		"dial orbstack ssh socket",
		"open orbstack ssh session",
		"create orbstack ssh session",
		"start orbstack ssh command",
		"connection refused",
		"connection reset",
		"connection lost",
		"handshake failed",
		"unexpected packet",
		"eof",
	}
	for _, pattern := range patterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func (c *VMClient) machineSSHUser(ctx context.Context, machine string) (string, error) {
	info, err := c.GetVMInfo(ctx, machine)
	if err != nil {
		if !errors.Is(err, ErrVMNotFound) {
			return "", err
		}
	}
	user := ""
	if err == nil {
		user = strings.TrimSpace(info.Config.DefaultUsername)
	}
	if user == "" {
		return strings.TrimSpace(machine), nil
	}
	return user + "@" + strings.TrimSpace(machine), nil
}

func (c *VMClient) orbClient() orbControl {
	if c != nil && c.orb != nil {
		return c.orb
	}
	return orbctl.NewClient(orbctl.DefaultSconRPCSocketPath())
}

func (c *VMClient) machineFilePath(machine string, vmPath string) (string, error) {
	machine = strings.TrimSpace(machine)
	vmPath = strings.TrimSpace(vmPath)
	if machine == "" {
		return "", fmt.Errorf("machine is required")
	}
	if vmPath == "" {
		return "", fmt.Errorf("path is required")
	}

	cleaned := path.Clean("/" + strings.TrimPrefix(vmPath, "/"))
	root := c.machineRoot(machine)
	if cleaned == "/" {
		return root, nil
	}
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(cleaned, "/")))
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	if cleanTarget != cleanRoot && !strings.HasPrefix(cleanTarget, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes orbstack vm root: %s", vmPath)
	}
	return cleanTarget, nil
}

func cleanVMPath(vmPath string) (string, error) {
	vmPath = strings.TrimSpace(vmPath)
	if vmPath == "" {
		return "", fmt.Errorf("path is required")
	}
	return path.Clean("/" + strings.TrimPrefix(vmPath, "/")), nil
}

func (c *VMClient) machineRoot(machine string) string {
	root := defaultOrbStackRoot()
	if c != nil && strings.TrimSpace(c.orbRoot) != "" {
		root = c.orbRoot
	}
	return filepath.Join(root, strings.TrimSpace(machine))
}

func vmInfoFromContainerInfo(info orbctl.ContainerInfo) VMInfo {
	vm := vmInfoFromContainerRecord(info.Record)
	if info.DiskSize != nil {
		vm.DiskSize = *info.DiskSize
	}
	vm.IP4 = strings.TrimSpace(info.IP4)
	vm.IP6 = strings.TrimSpace(info.IP6)
	return vm
}

func vmInfoFromContainerRecord(record orbctl.ContainerRecord) VMInfo {
	return VMInfo{
		ID:    record.ID,
		Name:  record.Name,
		State: record.State,
		Image: VMImage{
			Distro:  record.Image.Distro,
			Version: record.Image.Version,
			Arch:    record.Image.Arch,
			Variant: record.Image.Variant,
		},
		Config: VMConfig{
			Isolated:        record.Config.Isolated,
			ForwardSSHAgent: record.Config.ForwardSSHAgent,
			IsolateNetwork:  record.Config.IsolateNetwork,
			DefaultUsername: record.Config.DefaultUsername,
			HTTPPort:        record.Config.HTTPPort,
			HTTPSPort:       record.Config.HTTPSPort,
		},
		Builtin: record.Builtin,
	}
}

func mapOrbVMError(operation string, machine string, err error) error {
	if err == nil {
		return nil
	}
	if isVMNotFoundError(err) {
		return fmt.Errorf("%w: %s", ErrVMNotFound, machine)
	}
	return fmt.Errorf("%s orbstack vm %s: %w", operation, machine, err)
}

func isVMNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, orbctl.ErrContainerNotFound) {
		return true
	}
	return isVMNotFoundOutput(err.Error())
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

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
