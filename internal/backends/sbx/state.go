package sbxbackend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gateway "e2b-local/internal/gateway"
)

const sbxRuntimeStateDirectory = ".sbx-runtime"

// sbxRuntimeState is gateway recovery metadata, not a VM snapshot. sandboxd
// persists the microVM lifecycle but not e2b-local's bootstrap environment or
// reverse-tunnel credentials. We keep those values privately so a gateway
// restart can rediscover an existing sandbox and native stop/start can launch
// envd and its tunnel again. It does not make pause/resume faster and it cannot
// be used to create a new sandbox from captured memory or disk state.
type sbxRuntimeState struct {
	SandboxID           string             `json:"sandbox_id"`
	TemplateID          string             `json:"template_id"`
	Metadata            map[string]string  `json:"metadata"`
	Environment         map[string]string  `json:"environment"`
	Tunnels             []TunnelSpec       `json:"tunnels"`
	RuntimeInfo         SandboxRuntimeInfo `json:"runtime_info"`
	CreatedAt           time.Time          `json:"created_at"`
	EndAt               time.Time          `json:"end_at"`
	AllowInternetAccess *bool              `json:"allow_internet_access,omitempty"`
}

func newSbxRuntimeState(req SandboxRuntimeCreateRequest, info SandboxRuntimeInfo, environment map[string]string, tunnels []TunnelSpec) sbxRuntimeState {
	return sbxRuntimeState{
		SandboxID:           req.SandboxID,
		TemplateID:          req.TemplateID,
		Metadata:            gateway.CopyStringMap(req.Metadata),
		Environment:         gateway.CopyStringMap(environment),
		Tunnels:             copyTunnelSpecs(tunnels),
		RuntimeInfo:         copySbxRuntimeInfo(info),
		CreatedAt:           req.CreatedAt,
		EndAt:               req.EndAt,
		AllowInternetAccess: copyBool(req.AllowInternetAccess),
	}
}

func (s sbxRuntimeState) record(info SandboxRuntimeInfo, state string) SandboxRecord {
	return SandboxRecord{
		ID:                  s.SandboxID,
		TemplateID:          s.TemplateID,
		Metadata:            gateway.CopyStringMap(s.Metadata),
		EnvdURL:             info.EnvdURL,
		RuntimeInfo:         copySbxRuntimeInfo(info),
		CreatedAt:           s.CreatedAt,
		EndAt:               s.EndAt,
		State:               state,
		AllowInternetAccess: copyBool(s.AllowInternetAccess),
	}
}

func (r *SbxRuntime) saveRuntimeState(state sbxRuntimeState) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()

	path, err := r.runtimeStatePath(state.SandboxID)
	if err != nil {
		return fmt.Errorf("resolve sbx runtime state path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create sbx runtime state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode sbx runtime state: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("create sbx runtime state file: %w", err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect sbx runtime state: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write sbx runtime state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close sbx runtime state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish sbx runtime state: %w", err)
	}
	return nil
}

func (r *SbxRuntime) loadRuntimeState(sandboxID string) (sbxRuntimeState, bool, error) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	path, err := r.runtimeStatePath(sandboxID)
	if err != nil {
		return sbxRuntimeState{}, false, fmt.Errorf("resolve sbx runtime state path: %w", err)
	}
	state, found, err := readSbxRuntimeState(path)
	if err != nil {
		return sbxRuntimeState{}, false, fmt.Errorf("load sbx runtime state: %w", err)
	}
	if found && state.SandboxID != sandboxID {
		return sbxRuntimeState{}, false, fmt.Errorf("sbx runtime state sandbox id mismatch")
	}
	return state, found, nil
}

func (r *SbxRuntime) listRuntimeStates() ([]sbxRuntimeState, error) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()

	directory := filepath.Join(r.cfg.VolumeHostPath, sbxRuntimeStateDirectory)
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sbx runtime states: %w", err)
	}
	states := make([]sbxRuntimeState, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		state, found, err := readSbxRuntimeState(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("load sbx runtime state %s: %w", entry.Name(), err)
		}
		if found {
			states = append(states, state)
		}
	}
	return states, nil
}

func (r *SbxRuntime) removeRuntimeState(sandboxID string) error {
	if strings.TrimSpace(sandboxID) == "" {
		return nil
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	path, err := r.runtimeStatePath(sandboxID)
	if err != nil {
		return fmt.Errorf("resolve sbx runtime state path: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sbx runtime state: %w", err)
	}
	return nil
}

func (r *SbxRuntime) runtimeStatePath(sandboxID string) (string, error) {
	name := safeSbxImageComponent(sandboxID)
	if name == "" {
		return "", fmt.Errorf("sandbox id is required for sbx runtime state")
	}
	return filepath.Join(r.cfg.VolumeHostPath, sbxRuntimeStateDirectory, name+".json"), nil
}

func readSbxRuntimeState(path string) (sbxRuntimeState, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return sbxRuntimeState{}, false, nil
	}
	if err != nil {
		return sbxRuntimeState{}, false, fmt.Errorf("read sbx runtime state: %w", err)
	}
	var state sbxRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return sbxRuntimeState{}, false, fmt.Errorf("decode sbx runtime state: %w", err)
	}
	if strings.TrimSpace(state.SandboxID) == "" {
		return sbxRuntimeState{}, false, fmt.Errorf("sbx runtime state has no sandbox id")
	}
	return state, true, nil
}

func copySbxRuntimeInfo(info SandboxRuntimeInfo) SandboxRuntimeInfo {
	info.VolumeMounts = append([]VolumeMount(nil), info.VolumeMounts...)
	info.PublishedPorts = append([]gateway.SandboxPortMapping(nil), info.PublishedPorts...)
	return info
}

func copyTunnelSpecs(tunnels []TunnelSpec) []TunnelSpec {
	cloned := make([]TunnelSpec, len(tunnels))
	copy(cloned, tunnels)
	for index := range cloned {
		cloned[index].OnError = nil
	}
	return cloned
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
