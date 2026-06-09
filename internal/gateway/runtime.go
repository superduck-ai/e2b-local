package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

var ErrRuntimeCapacity = errors.New("runtime capacity exhausted")

type SandboxRuntime interface {
	CreateSandbox(ctx context.Context, req SandboxRuntimeCreateRequest) (SandboxRuntimeInfo, error)
	ListTemplates(ctx context.Context) ([]SandboxRuntimeTemplate, error)
	DeleteSandbox(ctx context.Context, info SandboxRuntimeInfo) error
	PauseSandbox(ctx context.Context, info SandboxRuntimeInfo) error
	ResumeSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInfo, error)
}

type SandboxRuntimeCreateRequest struct {
	SandboxID           string
	TemplateID          string
	Metadata            map[string]string
	EnvVars             map[string]string
	VolumeMounts        []VolumeMount
	CreatedAt           time.Time
	EndAt               time.Time
	AllowInternetAccess *bool
}

type SandboxRuntimeInfo struct {
	SandboxID     string
	EnvdURL       string
	ContainerID   string
	ContainerName string
	ContainerIP   string
	HostPort      string
	MachineID     string
	VolumeMounts  []VolumeMount
}

type SandboxRuntimeInspection struct {
	Info   SandboxRuntimeInfo
	State  string
	Exists bool
}

type SandboxRuntimeInspector interface {
	InspectSandbox(ctx context.Context, info SandboxRuntimeInfo) (SandboxRuntimeInspection, error)
}

type SandboxRuntimeRestorer interface {
	RestoreSandboxes(ctx context.Context) ([]SandboxRecord, error)
}

type SandboxRuntimeTemplate struct {
	TemplateID    string
	Names         []string
	ImageRef      string
	BuildCount    int
	BuildID       string
	BuildStatus   string
	CPUCount      int
	DiskSizeMB    int
	MemoryMB      int
	Public        bool
	SpawnCount    int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastSpawnedAt *time.Time
}

type VolumeRuntime interface {
	CreateVolume(ctx context.Context, name string) (RuntimeVolume, error)
	ListVolumes(ctx context.Context) ([]RuntimeVolume, error)
	GetVolume(ctx context.Context, volumeID string) (RuntimeVolume, error)
	DeleteVolume(ctx context.Context, volumeID string) (bool, error)
}

type RuntimeVolume struct {
	VolumeID string
	Name     string
}

type SandboxRuntimeFactory func(cfg Config, logger *log.Logger) (SandboxRuntime, error)

var sandboxRuntimeFactories = map[string]SandboxRuntimeFactory{}

func RegisterSandboxRuntimeFactory(runtimeType string, factory SandboxRuntimeFactory) {
	runtimeType = strings.TrimSpace(runtimeType)
	if runtimeType == "" {
		panic("runtime type is required")
	}
	if factory == nil {
		panic("runtime factory is required")
	}
	sandboxRuntimeFactories[runtimeType] = factory
}

func NewSandboxRuntime(cfg Config, logger *log.Logger) (SandboxRuntime, error) {
	if factory, ok := sandboxRuntimeFactories[cfg.Runtime.Type]; ok {
		return factory(cfg, logger)
	}
	return nil, fmt.Errorf("unsupported runtime type %q", cfg.Runtime.Type)
}
