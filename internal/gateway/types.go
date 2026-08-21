package gateway

import (
	"fmt"
	"time"
)

type NewSandboxRequest struct {
	TemplateID          string            `json:"templateID"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	EnvVars             map[string]string `json:"envVars,omitempty"`
	Timeout             int               `json:"timeout,omitempty"`
	Secure              *bool             `json:"secure,omitempty"`
	AllowInternetAccess *bool             `json:"allow_internet_access,omitempty"`
	VolumeMounts        []VolumeMount     `json:"volumeMounts,omitempty"`
}

type SandboxResponse struct {
	Alias        string        `json:"alias,omitempty"`
	ClientID     string        `json:"clientID"`
	Domain       *string       `json:"domain"`
	EnvdURL      string        `json:"envdURL,omitempty"`
	EnvdVersion  string        `json:"envdVersion"`
	SandboxID    string        `json:"sandboxID"`
	TemplateID   string        `json:"templateID"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
}

type ListedSandboxResponse struct {
	Alias        string            `json:"alias,omitempty"`
	ClientID     string            `json:"clientID"`
	CPUCount     int               `json:"cpuCount"`
	DiskSizeMB   int               `json:"diskSizeMB"`
	EndAt        time.Time         `json:"endAt"`
	EnvdVersion  string            `json:"envdVersion"`
	MemoryMB     int               `json:"memoryMB"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	SandboxID    string            `json:"sandboxID"`
	StartedAt    time.Time         `json:"startedAt"`
	State        string            `json:"state"`
	TemplateID   string            `json:"templateID"`
	VolumeMounts []VolumeMount     `json:"volumeMounts,omitempty"`
}

type TemplateResponse struct {
	TemplateID    string     `json:"templateID"`
	Aliases       []string   `json:"aliases"`
	Names         []string   `json:"names"`
	ImageRef      string     `json:"imageRef,omitempty"`
	BuildCount    int        `json:"buildCount"`
	BuildID       string     `json:"buildID"`
	BuildStatus   string     `json:"buildStatus"`
	CPUCount      int        `json:"cpuCount"`
	DiskSizeMB    int        `json:"diskSizeMB"`
	EnvdVersion   string     `json:"envdVersion"`
	LastSpawnedAt *time.Time `json:"lastSpawnedAt"`
	MemoryMB      int        `json:"memoryMB"`
	Public        bool       `json:"public"`
	SpawnCount    int64      `json:"spawnCount"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type VolumeMount struct {
	Name      string `json:"name,omitempty"`
	Path      string `json:"path,omitempty"`
	VolumeID  string `json:"volumeID,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
}

type VolumeResponse struct {
	VolumeID string `json:"volumeID"`
	Name     string `json:"name"`
	Token    string `json:"token,omitempty"`
}

type SandboxPortResponse struct {
	ContainerPort int    `json:"containerPort"`
	Host          string `json:"host"`
	HostPort      int    `json:"hostPort"`
	URL           string `json:"url"`
	WSURL         string `json:"wsUrl"`
	Protocol      string `json:"protocol,omitempty"`
}

type InternetAccessPolicy string

const (
	InternetAccessUnspecified InternetAccessPolicy = ""
	InternetAccessAllowed     InternetAccessPolicy = "allowed"
	InternetAccessDenied      InternetAccessPolicy = "denied"
)

func InternetAccessPolicyFromBoolPtr(value *bool) InternetAccessPolicy {
	if value == nil {
		return InternetAccessUnspecified
	}
	if *value {
		return InternetAccessAllowed
	}
	return InternetAccessDenied
}

func (policy InternetAccessPolicy) BoolPtr() *bool {
	switch policy {
	case InternetAccessAllowed:
		value := true
		return &value
	case InternetAccessDenied:
		value := false
		return &value
	default:
		return nil
	}
}

// SandboxTimeoutAction describes the action taken when a running sandbox reaches EndAt.
// The unspecified value is reserved for partial updates and legacy persisted records.
type SandboxTimeoutAction string

const (
	SandboxTimeoutActionUnspecified SandboxTimeoutAction = ""
	SandboxTimeoutActionKill        SandboxTimeoutAction = "kill"
	SandboxTimeoutActionPause       SandboxTimeoutAction = "pause"
)

func SandboxTimeoutActionFromAutoPause(autoPause *bool) SandboxTimeoutAction {
	if autoPause != nil && *autoPause {
		return SandboxTimeoutActionPause
	}
	return SandboxTimeoutActionKill
}

// Normalize applies the backward-compatible default and rejects unknown actions.
func (action SandboxTimeoutAction) Normalize() (SandboxTimeoutAction, error) {
	switch action {
	case SandboxTimeoutActionUnspecified, SandboxTimeoutActionKill:
		return SandboxTimeoutActionKill, nil
	case SandboxTimeoutActionPause:
		return SandboxTimeoutActionPause, nil
	default:
		return SandboxTimeoutActionUnspecified, fmt.Errorf("unsupported sandbox timeout action %q", action)
	}
}

// RetainsSandboxAfterTimeout reports whether timeout is a recoverable lifecycle transition.
func (action SandboxTimeoutAction) RetainsSandboxAfterTimeout() bool {
	normalized, err := action.Normalize()
	return err == nil && normalized == SandboxTimeoutActionPause
}

type SandboxRecord struct {
	ID                   string
	TemplateID           string
	Alias                string
	ClientID             string
	EnvdVersion          string
	Metadata             map[string]string
	EnvdURL              string
	RuntimeInfo          SandboxRuntimeInfo
	CreatedAt            time.Time
	EndAt                time.Time
	State                string
	CPUCount             int32
	DiskSizeMB           int32
	MemoryMB             int32
	InternetAccessPolicy InternetAccessPolicy
	OnTimeout            SandboxTimeoutAction
}
