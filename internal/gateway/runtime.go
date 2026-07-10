package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	SandboxID      string
	EnvdURL        string
	ContainerID    string
	ContainerName  string
	ContainerIP    string
	HostPort       string
	MachineID      string
	VolumeMounts   []VolumeMount
	PublishedPorts []SandboxPortMapping
}

type SandboxPortMapping struct {
	ContainerPort int
	HostIP        string
	HostPort      int
	Protocol      string
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

// VolumeContentRuntime 定义卷内文件和目录操作；具体实现负责路径边界与原子写入。
type VolumeContentRuntime interface {
	GetVolumePathInfo(ctx context.Context, volumeID string, path string) (VolumeEntryStat, error)
	ReadVolumeFile(ctx context.Context, volumeID string, path string) (io.ReadCloser, error)
	WriteVolumeFile(ctx context.Context, volumeID string, path string, body io.Reader, opts VolumeWriteOptions) (VolumeEntryStat, error)
	ListVolumeDir(ctx context.Context, volumeID string, path string, depth int) ([]VolumeEntryStat, error)
	CreateVolumeDir(ctx context.Context, volumeID string, path string, opts VolumeWriteOptions) (VolumeEntryStat, error)
}

type RuntimeVolume struct {
	VolumeID string
	Name     string
}

// VolumeEntryStat 是卷内容 API 返回的可序列化文件属性。
// 不支持原生 stat 字段的平台会让 UID/GID 保持零值，并以 mtime 回退 atime/ctime。
type VolumeEntryStat struct {
	Atime  time.Time `json:"atime"`
	Mtime  time.Time `json:"mtime"`
	Ctime  time.Time `json:"ctime"`
	Type   string    `json:"type"`
	Name   string    `json:"name"`
	Path   string    `json:"path"`
	Size   int64     `json:"size"`
	UID    int       `json:"uid"`
	GID    int       `json:"gid"`
	Mode   int       `json:"mode"`
	Target string    `json:"target,omitempty"`
}

// VolumeWriteOptions 使用指针区分“未指定”与显式设置为零的权限或属主值。
type VolumeWriteOptions struct {
	Force bool
	Mode  *int
	UID   *int
	GID   *int
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
