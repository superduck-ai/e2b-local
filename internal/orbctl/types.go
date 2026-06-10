package orbctl

type ContainerInfo struct {
	Record   ContainerRecord `json:"record"`
	DiskSize *int64          `json:"disk_size,omitempty"`
	IP4      string          `json:"ip4,omitempty"`
	IP6      string          `json:"ip6,omitempty"`
}

type ContainerRecord struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Image   ContainerImage  `json:"image"`
	Config  ContainerConfig `json:"config"`
	Builtin bool            `json:"builtin"`
	State   string          `json:"state"`
}

type ContainerImage struct {
	Distro  string `json:"distro"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Variant string `json:"variant"`
}

type ContainerConfig struct {
	Isolated        bool           `json:"isolated"`
	ForwardSSHAgent bool           `json:"forward_ssh_agent"`
	IsolateNetwork  bool           `json:"isolate_network"`
	DefaultUsername string         `json:"default_username"`
	HTTPPort        int            `json:"http_port"`
	HTTPSPort       int            `json:"https_port"`
	Mounts          []MachineMount `json:"mounts,omitempty"`
}

type MachineMount struct {
	Source      string `json:"source"`
	Destination string `json:"destination,omitempty"`
}
