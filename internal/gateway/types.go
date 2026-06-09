package gateway

import "time"

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

type SandboxRecord struct {
	ID                  string
	TemplateID          string
	Alias               *string
	ClientID            string
	EnvdVersion         string
	Metadata            map[string]string
	EnvdURL             string
	RuntimeInfo         SandboxRuntimeInfo
	CreatedAt           time.Time
	EndAt               time.Time
	State               string
	CPUCount            int32
	DiskSizeMB          int32
	MemoryMB            int32
	AllowInternetAccess *bool
}
