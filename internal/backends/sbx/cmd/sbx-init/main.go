package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	sbxbackend "e2b-local/internal/backends/sbx"
	"golang.org/x/sys/unix"
)

const (
	bootstrapVolumesEnv = "E2B_SBX_BOOTSTRAP_VOLUMES"
	tunnelsEnv          = "E2B_SBX_TUNNELS"
	startCommandEnv     = "E2B_SBX_ENVD_START_CMD"
	envdPortEnv         = "E2B_SBX_ENVD_PORT"
)

type volumeSpec struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sbx-init: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := redirectLogs(); err != nil {
		return fmt.Errorf("redirect logs: %w", err)
	}
	if err := installVolumeMounts(os.Getenv(bootstrapVolumesEnv)); err != nil {
		return fmt.Errorf("install volume mounts: %w", err)
	}
	if err := startTunnels(os.Getenv(tunnelsEnv)); err != nil {
		return fmt.Errorf("start tunnels: %w", err)
	}
	if command := strings.TrimSpace(os.Getenv(startCommandEnv)); command != "" {
		if err := startCommand(command); err != nil {
			return fmt.Errorf("start configured command: %w", err)
		}
	}

	port, err := envdPort()
	if err != nil {
		return fmt.Errorf("resolve envd port: %w", err)
	}
	arguments := []string{"/usr/local/bin/envd", "-isnotfc", "-port", strconv.Itoa(port)}
	if err := syscall.Exec(arguments[0], arguments, os.Environ()); err != nil {
		return fmt.Errorf("exec envd: %w", err)
	}
	return nil
}

func redirectLogs() error {
	if err := os.MkdirAll("/var/log", 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	logFile, err := os.OpenFile("/var/log/envd.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open envd log: %w", err)
	}
	if err := unix.Dup2(int(logFile.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("redirect stdout: %w", err)
	}
	if err := unix.Dup2(int(logFile.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("redirect stderr: %w", err)
	}
	return logFile.Close()
}

func installVolumeMounts(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var volumes []volumeSpec
	if err := json.Unmarshal([]byte(raw), &volumes); err != nil {
		return fmt.Errorf("decode bootstrap volumes: %w", err)
	}
	for _, volume := range volumes {
		source := filepath.Clean(strings.TrimSpace(volume.Source))
		target := filepath.Clean(strings.TrimSpace(volume.Target))
		if !filepath.IsAbs(source) || !filepath.IsAbs(target) || target == "/" || isProtectedTarget(target) {
			return fmt.Errorf("invalid bootstrap volume target %q", volume.Target)
		}
		info, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("inspect bootstrap volume source %s: %w", source, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("bootstrap volume source is not a directory: %s", source)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create bootstrap volume parent %s: %w", filepath.Dir(target), err)
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("clear bootstrap volume target %s: %w", target, err)
		}
		if err := os.Symlink(source, target); err != nil {
			return fmt.Errorf("link bootstrap volume %s to %s: %w", target, source, err)
		}
	}
	return nil
}

func isProtectedTarget(target string) bool {
	firstSegment := strings.Split(strings.TrimPrefix(target, "/"), "/")[0]
	switch firstSegment {
	case "bin", "dev", "etc", "lib", "proc", "sbin", "sys", "usr":
		return true
	default:
		return false
	}
}

func startTunnels(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var tunnels []sbxbackend.TunnelSpec
	if err := json.Unmarshal([]byte(raw), &tunnels); err != nil {
		return fmt.Errorf("decode tunnel configuration: %w", err)
	}
	for _, tunnel := range tunnels {
		arguments := []string{
			"-relay", tunnel.RelayAddress,
			"-token", tunnel.Token,
			"-target-port", strconv.Itoa(tunnel.TargetPort),
			"-connections", strconv.Itoa(tunnel.Connections),
		}
		command := exec.Command("/usr/local/bin/sbx-tunnel", arguments...)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			return fmt.Errorf("start tunnel for port %d: %w", tunnel.TargetPort, err)
		}
	}
	return nil
}

func startCommand(command string) error {
	process := exec.Command("/bin/sh", "-lc", command)
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		return fmt.Errorf("spawn configured command: %w", err)
	}
	return nil
}

func envdPort() (int, error) {
	value := strings.TrimSpace(os.Getenv(envdPortEnv))
	if value == "" {
		return 49983, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid %s value %q", envdPortEnv, value)
	}
	return port, nil
}
