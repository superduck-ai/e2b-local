package dockerbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	gateway "e2b-local/internal/gateway"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
	"github.com/google/uuid"
)

// 卷内容位于 <root>/<volumeID>，管理元数据单独放在 <root>/.metadata，避免暴露给 sandbox。
const (
	maxDockerVolumeListDepth        = 10
	maxDockerVolumeIDLength         = 128
	dockerLocalVolumeMetadataDir    = ".metadata"
	dockerLocalVolumeMetadataSuffix = ".json"
)

type dockerLocalVolumeMetadata struct {
	VolumeID  string    `json:"volume_id"`
	Name      string    `json:"name"`
	Managed   bool      `json:"managed"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *DockerRuntime) CreateVolume(_ context.Context, name string) (RuntimeVolume, error) {
	r.volumeCreateMu.Lock()
	defer r.volumeCreateMu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return RuntimeVolume{}, fmt.Errorf("volume name is required")
	}
	_, volumeName, dir, metadataPath, err := r.localVolumePaths(name)
	if err != nil {
		return RuntimeVolume{}, err
	}
	if info, err := os.Lstat(dir); err == nil {
		// 已有目录只有携带有效受管元数据时才可复用，避免接管用户预先创建的目录。
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return RuntimeVolume{}, fmt.Errorf("docker local volume path %s is not a directory", dir)
		}
		volume, err := readDockerLocalVolumeMetadata(dir, metadataPath)
		if err != nil {
			return RuntimeVolume{}, fmt.Errorf("refusing to use unmanaged docker local volume directory %s", dir)
		}
		if volume.VolumeID != volumeName {
			return RuntimeVolume{}, fmt.Errorf("docker local volume metadata mismatch for %s", name)
		}
		return volume, nil
	} else if !os.IsNotExist(err) {
		return RuntimeVolume{}, fmt.Errorf("stat docker local volume %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return RuntimeVolume{}, fmt.Errorf("create docker local volume %s: %w", name, err)
	}
	metadata := dockerLocalVolumeMetadata{
		VolumeID:  volumeName,
		Name:      volumeName,
		Managed:   true,
		CreatedAt: time.Now().UTC(),
	}
	if err := writeDockerLocalVolumeMetadata(metadataPath, metadata); err != nil {
		_ = os.RemoveAll(dir)
		return RuntimeVolume{}, err
	}
	return RuntimeVolume{VolumeID: volumeName, Name: volumeName}, nil
}

func (r *DockerRuntime) ListVolumes(_ context.Context) ([]RuntimeVolume, error) {
	base, err := r.ensureLocalVolumeRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("list docker local volumes: %w", err)
	}
	result := make([]RuntimeVolume, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// 大小写不敏感文件系统会把 .METADATA 与 .metadata 视为同一目录。
		if isDockerLocalVolumeReservedName(entry.Name()) {
			continue
		}
		metadataPath := filepath.Join(base, dockerLocalVolumeMetadataDir, entry.Name()+dockerLocalVolumeMetadataSuffix)
		volume, err := readDockerLocalVolumeMetadata(filepath.Join(base, entry.Name()), metadataPath)
		if err != nil {
			continue
		}
		if volume.VolumeID != entry.Name() {
			continue
		}
		result = append(result, volume)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (r *DockerRuntime) GetVolume(_ context.Context, volumeID string) (RuntimeVolume, error) {
	volume, _, err := r.localVolume(volumeID)
	return volume, err
}

func (r *DockerRuntime) DeleteVolume(ctx context.Context, volumeID string) (bool, error) {
	// 写锁与 CreateSandbox 的读锁配对，保证“检查引用并删除”不会和容器创建交错。
	r.volumeMountMu.Lock()
	defer r.volumeMountMu.Unlock()

	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	inUse, err := r.dockerLocalVolumeInUse(ctx, dir)
	if err != nil {
		return false, fmt.Errorf("check docker local volume %s usage: %w", volumeID, err)
	}
	if inUse {
		return false, gateway.NewGatewayError(http.StatusConflict, "volume %s is in use", volumeID)
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove docker local volume %s: %w", volumeID, err)
	}
	if metadataPath, err := r.localVolumeMetadataPath(volumeID); err == nil {
		if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("remove docker local volume metadata %s: %w", volumeID, err)
		}
	}
	return true, nil
}

func (r *DockerRuntime) dockerLocalVolumeInUse(ctx context.Context, dir string) (bool, error) {
	if r.client == nil {
		return false, fmt.Errorf("docker client is unavailable")
	}
	// 停止的容器仍保留挂载引用，因此必须查询全部容器而不只是运行中的容器。
	containers, err := r.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return false, fmt.Errorf("list docker containers: %w", err)
	}
	for _, containerInfo := range containers {
		for _, mountPoint := range containerInfo.Mounts {
			if mountPoint.Type == mount.TypeBind && dockerBindMountSourceMatches(mountPoint.Source, dir) {
				return true, nil
			}
		}
	}
	return false, nil
}

func dockerBindMountSourceMatches(source string, dir string) bool {
	source = filepath.Clean(strings.TrimSpace(source))
	dir = filepath.Clean(strings.TrimSpace(dir))
	if source == dir {
		return true
	}
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		// 默认 macOS/Windows 文件系统不区分大小写，路径比较需保持相同语义。
		return strings.EqualFold(source, dir)
	}
	return false
}

func (r *DockerRuntime) GetVolumePathInfo(_ context.Context, volumeID string, path string) (gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	rel, rootPath, err := resolveVolumeContentPath(path, true)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	root, err := openVolumeContentRoot(dir)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	defer func() { _ = root.Close() }()
	return volumeEntryStat(root, rootPath, rel)
}

func (r *DockerRuntime) ReadVolumeFile(_ context.Context, volumeID string, path string) (io.ReadCloser, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return nil, err
	}
	_, rootPath, err := resolveVolumeContentPath(path, false)
	if err != nil {
		return nil, err
	}
	root, err := openVolumeContentRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(rootPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFound(fmt.Errorf("volume file not found"))
		}
		return nil, wrapVolumeContentPathError("read volume file", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat volume file: %w", err)
	}
	if info.IsDir() {
		_ = file.Close()
		return nil, gateway.NewGatewayError(http.StatusBadRequest, "path is a directory")
	}
	return file, nil
}

func (r *DockerRuntime) WriteVolumeFile(_ context.Context, volumeID string, path string, body io.Reader, opts gateway.VolumeWriteOptions) (gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	rel, _, err := resolveVolumeContentPath(path, false)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	root, err := openVolumeContentRoot(dir)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	defer func() { _ = root.Close() }()

	parentPath := volumeRootPath(pathpkg.Dir(rel))
	if err := root.MkdirAll(parentPath, 0o755); err != nil {
		if os.IsExist(err) {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path parent is not a directory")
		}
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("create volume file parent", err)
	}
	parentRoot, err := root.OpenRoot(parentPath)
	if err != nil {
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("open volume file parent", err)
	}
	defer func() { _ = parentRoot.Close() }()
	// 后续操作锚定在已打开的父目录，sandbox 即使替换路径组件也无法把写入引到卷外。
	targetName := filepath.FromSlash(pathpkg.Base(rel))

	if info, err := parentRoot.Lstat(targetName); err == nil {
		if info.IsDir() {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path is a directory")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path is a symbolic link")
		}
		if !opts.Force {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusConflict, "path already exists")
		}
	} else if !os.IsNotExist(err) {
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("stat volume file", err)
	}
	mode := os.FileMode(0o644)
	if opts.Mode != nil {
		mode = os.FileMode(*opts.Mode)
	}
	// 先完整写入并同步临时文件，上传失败时旧文件和目标路径都不会留下半截内容。
	tmpName, err := writeVolumeTempFile(parentRoot, body, mode, opts)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	defer func() {
		_ = parentRoot.Remove(tmpName)
	}()
	if opts.Force {
		if err := parentRoot.Rename(tmpName, targetName); err != nil {
			return gateway.VolumeEntryStat{}, fmt.Errorf("replace volume file: %w", err)
		}
	} else {
		// hard link 提供原子的“不存在才创建”语义，避免预检查与发布之间的竞态。
		if err := parentRoot.Link(tmpName, targetName); err != nil {
			if os.IsExist(err) {
				return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusConflict, "path already exists")
			}
			return gateway.VolumeEntryStat{}, fmt.Errorf("create volume file: %w", err)
		}
	}
	return volumeEntryStat(parentRoot, targetName, rel)
}

func (r *DockerRuntime) ListVolumeDir(_ context.Context, volumeID string, path string, depth int) ([]gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return nil, err
	}
	rel, rootPath, err := resolveVolumeContentPath(path, true)
	if err != nil {
		return nil, err
	}
	root, err := openVolumeContentRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(rootPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFound(fmt.Errorf("volume directory not found"))
		}
		return nil, wrapVolumeContentPathError("stat volume directory", err)
	}
	if !info.IsDir() {
		return nil, gateway.NewGatewayError(http.StatusBadRequest, "path is not a directory")
	}
	if depth <= 0 {
		depth = 1
	}
	if depth > maxDockerVolumeListDepth {
		return nil, gateway.NewGatewayError(http.StatusBadRequest, "volume directory depth must be less than or equal to %d", maxDockerVolumeListDepth)
	}
	dirRoot, err := root.OpenRoot(rootPath)
	if err != nil {
		return nil, wrapVolumeContentPathError("open volume directory", err)
	}
	defer func() { _ = dirRoot.Close() }()
	var result []gateway.VolumeEntryStat
	if err := collectVolumeDirEntries(dirRoot, rel, depth, &result); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func (r *DockerRuntime) CreateVolumeDir(_ context.Context, volumeID string, path string, opts gateway.VolumeWriteOptions) (gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	rel, rootPath, err := resolveVolumeContentPath(path, true)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	root, err := openVolumeContentRoot(dir)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	defer func() { _ = root.Close() }()
	mode := os.FileMode(0o755)
	if opts.Mode != nil {
		mode = os.FileMode(*opts.Mode)
	}
	if info, err := root.Lstat(rootPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path is a symbolic link")
		}
		if !info.IsDir() {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path is not a directory")
		}
	} else if !os.IsNotExist(err) {
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("stat volume directory", err)
	}
	if err := root.MkdirAll(rootPath, mode); err != nil {
		if os.IsExist(err) {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path parent is not a directory")
		}
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("create volume directory", err)
	}
	dirFile, err := root.Open(rootPath)
	if err != nil {
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("open volume directory", err)
	}
	defer func() { _ = dirFile.Close() }()
	if opts.Mode != nil {
		if err := dirFile.Chmod(mode); err != nil {
			return gateway.VolumeEntryStat{}, fmt.Errorf("chmod volume directory: %w", err)
		}
	}
	if err := applyVolumeOwnership(dirFile, opts, "volume directory"); err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	info, err := dirFile.Stat()
	if err != nil {
		return gateway.VolumeEntryStat{}, fmt.Errorf("stat volume directory: %w", err)
	}
	return volumeEntryStatFromInfo(root, rootPath, rel, info)
}

func (r *DockerRuntime) localVolume(volumeID string) (RuntimeVolume, string, error) {
	_, _, dir, metadataPath, err := r.localVolumePaths(volumeID)
	if err != nil {
		return RuntimeVolume{}, "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return RuntimeVolume{}, "", errdefs.NotFound(fmt.Errorf("volume %s not found", volumeID))
		}
		return RuntimeVolume{}, "", fmt.Errorf("stat docker local volume %s: %w", volumeID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return RuntimeVolume{}, "", errdefs.NotFound(fmt.Errorf("volume %s not found", volumeID))
	}
	volume, err := readDockerLocalVolumeMetadata(dir, metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return RuntimeVolume{}, "", errdefs.NotFound(fmt.Errorf("volume %s not found", volumeID))
		}
		return RuntimeVolume{}, "", err
	}
	if volume.VolumeID != strings.TrimSpace(volumeID) {
		return RuntimeVolume{}, "", errdefs.NotFound(fmt.Errorf("volume %s not found", volumeID))
	}
	return volume, dir, nil
}

func (r *DockerRuntime) ensureLocalVolume(volumeID string) (RuntimeVolume, string, error) {
	volume, dir, err := r.localVolume(volumeID)
	if err == nil {
		return volume, dir, nil
	}
	if !errdefs.IsNotFound(err) {
		return RuntimeVolume{}, "", err
	}
	// 挂载请求沿用“首次引用即创建”的本地卷语义，并由 CreateVolume 写入受管元数据。
	volume, err = r.CreateVolume(context.Background(), volumeID)
	if err != nil {
		return RuntimeVolume{}, "", err
	}
	dir, err = r.localVolumeHostDir(volume.VolumeID)
	if err != nil {
		return RuntimeVolume{}, "", err
	}
	return volume, dir, nil
}

func (r *DockerRuntime) ensureLocalVolumeRoot() (string, error) {
	root := strings.TrimSpace(r.cfg.VolumeHostPath)
	if root == "" {
		return "", fmt.Errorf("docker.volume_host_path is required")
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("docker.volume_host_path must be an absolute path")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create docker local volume root: %w", err)
	}
	return root, nil
}

func (r *DockerRuntime) localVolumeHostDir(volumeID string) (string, error) {
	_, _, dir, _, err := r.localVolumePaths(volumeID)
	return dir, err
}

func (r *DockerRuntime) localVolumeMetadataPath(volumeID string) (string, error) {
	_, _, _, metadataPath, err := r.localVolumePaths(volumeID)
	return metadataPath, err
}

func (r *DockerRuntime) localVolumePaths(volumeID string) (string, string, string, string, error) {
	root, err := r.ensureLocalVolumeRoot()
	if err != nil {
		return "", "", "", "", err
	}
	name, err := dockerLocalVolumeDirName(volumeID)
	if err != nil {
		return "", "", "", "", err
	}
	dir := filepath.Join(root, name)
	if err := ensurePathLexicallyWithinRoot(root, dir); err != nil {
		return "", "", "", "", err
	}
	metadataPath := filepath.Join(root, dockerLocalVolumeMetadataDir, name+dockerLocalVolumeMetadataSuffix)
	if err := ensurePathLexicallyWithinRoot(root, metadataPath); err != nil {
		return "", "", "", "", err
	}
	return root, name, dir, metadataPath, nil
}

func dockerLocalVolumeDirName(volumeID string) (string, error) {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return "", fmt.Errorf("volume id is required")
	}
	if len(volumeID) > maxDockerVolumeIDLength {
		return "", gateway.NewGatewayError(http.StatusBadRequest, "volume id is too long")
	}
	if strings.Contains(volumeID, "\x00") || strings.ContainsAny(volumeID, `/\`) || volumeID == "." || volumeID == ".." || isDockerLocalVolumeReservedName(volumeID) {
		return "", gateway.NewGatewayError(http.StatusBadRequest, "invalid volume id")
	}
	for _, r := range volumeID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return "", gateway.NewGatewayError(http.StatusBadRequest, "invalid volume id")
	}
	return volumeID, nil
}

func isDockerLocalVolumeReservedName(name string) bool {
	return strings.EqualFold(name, dockerLocalVolumeMetadataDir) || strings.EqualFold(name, dockerLocalVolumeMetadataFile)
}

func readDockerLocalVolumeMetadata(dir string, metadataPath string) (RuntimeVolume, error) {
	metadata, err := readDockerLocalVolumeMetadataFile(metadataPath)
	if err == nil {
		return runtimeVolumeFromDockerLocalMetadata(metadata)
	}
	if !os.IsNotExist(err) {
		return RuntimeVolume{}, err
	}

	// 兼容早期版本：读取内容目录中的旧标记后迁移到不会被挂载的元数据目录。
	legacyPath := filepath.Join(dir, dockerLocalVolumeMetadataFile)
	metadata, err = readDockerLocalVolumeMetadataFile(legacyPath)
	if err != nil {
		return RuntimeVolume{}, err
	}
	if err := writeDockerLocalVolumeMetadata(metadataPath, metadata); err != nil {
		return RuntimeVolume{}, fmt.Errorf("migrate docker local volume metadata: %w", err)
	}
	_ = os.Remove(legacyPath)
	return runtimeVolumeFromDockerLocalMetadata(metadata)
}

func readDockerLocalVolumeMetadataFile(path string) (dockerLocalVolumeMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return dockerLocalVolumeMetadata{}, err
	}
	var metadata dockerLocalVolumeMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return dockerLocalVolumeMetadata{}, fmt.Errorf("decode docker local volume metadata: %w", err)
	}
	return metadata, nil
}

func runtimeVolumeFromDockerLocalMetadata(metadata dockerLocalVolumeMetadata) (RuntimeVolume, error) {
	if !metadata.Managed || strings.TrimSpace(metadata.VolumeID) == "" || strings.TrimSpace(metadata.Name) == "" {
		return RuntimeVolume{}, fmt.Errorf("invalid docker local volume metadata")
	}
	return RuntimeVolume{VolumeID: strings.TrimSpace(metadata.VolumeID), Name: strings.TrimSpace(metadata.Name)}, nil
}

func writeDockerLocalVolumeMetadata(metadataPath string, metadata dockerLocalVolumeMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(metadataPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write docker local volume metadata: %w", err)
	}
	return nil
}

func resolveVolumeContentPath(rawPath string, allowRoot bool) (string, string, error) {
	rel, err := cleanVolumeContentPath(rawPath, allowRoot)
	if err != nil {
		return "", "", err
	}
	if err := ensureVolumeContentPathAllowed(rel); err != nil {
		return "", "", err
	}
	return rel, volumeRootPath(rel), nil
}

// openVolumeContentRoot 将后续文件操作限制在卷根目录，阻止符号链接和并发换目录逃逸。
func openVolumeContentRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFound(fmt.Errorf("volume not found"))
		}
		return nil, fmt.Errorf("open volume root: %w", err)
	}
	return root, nil
}

func volumeRootPath(rel string) string {
	if rel == "" {
		return "."
	}
	return filepath.FromSlash(rel)
}

func wrapVolumeContentPathError(action string, err error) error {
	if isVolumeContentPathEscape(err) {
		return gateway.NewGatewayError(http.StatusBadRequest, "path escapes volume root")
	}
	return fmt.Errorf("%s: %w", action, err)
}

func isVolumeContentPathEscape(err error) bool {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Err != nil {
		return pathErr.Err.Error() == "path escapes from parent"
	}
	return false
}

// cleanVolumeContentPath 统一路径分隔符并拒绝上跳，同时原样保留 Unix 合法的首尾空白字符。
func cleanVolumeContentPath(rawPath string, allowRoot bool) (string, error) {
	if rawPath == "" {
		if allowRoot {
			return "", nil
		}
		return "", gateway.NewGatewayError(http.StatusBadRequest, "path is required")
	}
	if strings.Contains(rawPath, "\x00") {
		return "", gateway.NewGatewayError(http.StatusBadRequest, "path contains NUL")
	}
	rawPath = strings.ReplaceAll(rawPath, "\\", "/")
	parts := strings.Split(rawPath, "/")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", gateway.NewGatewayError(http.StatusBadRequest, "path must not contain ..")
		default:
			cleaned = append(cleaned, part)
		}
	}
	rel := pathpkg.Join(cleaned...)
	if rel == "." {
		rel = ""
	}
	if rel == "" && !allowRoot {
		return "", gateway.NewGatewayError(http.StatusBadRequest, "path is required")
	}
	return rel, nil
}

func ensureVolumeContentPathAllowed(rel string) error {
	if rel == "" {
		return nil
	}
	// 保留名按不区分大小写处理，兼容默认 macOS/Windows 文件系统语义。
	reserved := strings.ToLower(dockerLocalVolumeMetadataFile)
	normalized := strings.ToLower(rel)
	if normalized == reserved || strings.HasPrefix(normalized, reserved+"/") {
		return gateway.NewGatewayError(http.StatusBadRequest, "path is reserved")
	}
	return nil
}

func ensurePathLexicallyWithinRoot(root string, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return nil
	}
	return gateway.NewGatewayError(http.StatusBadRequest, "path escapes volume root")
}

func applyVolumeOwnership(file *os.File, opts gateway.VolumeWriteOptions, label string) error {
	if opts.UID == nil && opts.GID == nil {
		return nil
	}
	if goruntime.GOOS == "windows" {
		return nil
	}
	uid := -1
	gid := -1
	if opts.UID != nil {
		uid = *opts.UID
	}
	if opts.GID != nil {
		gid = *opts.GID
	}
	if err := file.Chown(uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", label, err)
	}
	return nil
}

// writeVolumeTempFile 在目标目录内完成写入、权限设置与 fsync，成功后才交给调用方发布。
func writeVolumeTempFile(root *os.Root, body io.Reader, mode os.FileMode, opts gateway.VolumeWriteOptions) (string, error) {
	var file *os.File
	var tmpName string
	for range 10 {
		tmpName = ".e2b-write-" + uuid.NewString()
		var err error
		file, err = root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("create temporary volume file: %w", err)
		}
	}
	if file == nil {
		return "", fmt.Errorf("create temporary volume file: name collision")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(file, body); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write volume file: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("chmod volume file: %w", err)
	}
	if err := applyVolumeOwnership(file, opts, "volume file"); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("sync volume file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close volume file: %w", err)
	}
	cleanup = false
	return tmpName, nil
}

// writeFileAtomic 使用同目录临时文件加 rename，避免读到截断或半写入的元数据。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	cleanup = false
	return nil
}

func volumeEntryStat(root *os.Root, rootPath string, rel string) (gateway.VolumeEntryStat, error) {
	info, err := root.Lstat(rootPath)
	if err != nil {
		if os.IsNotExist(err) {
			return gateway.VolumeEntryStat{}, errdefs.NotFound(fmt.Errorf("volume path not found"))
		}
		return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("stat volume path", err)
	}
	return volumeEntryStatFromInfo(root, rootPath, rel, info)
}

func volumeEntryStatFromInfo(root *os.Root, rootPath string, rel string, info os.FileInfo) (gateway.VolumeEntryStat, error) {
	mtime := info.ModTime()
	stat := gateway.VolumeEntryStat{
		Atime: mtime,
		Mtime: mtime,
		Ctime: mtime,
		Type:  "unknown",
		Name:  pathpkg.Base(rel),
		Path:  "/" + rel,
		Size:  info.Size(),
		Mode:  int(info.Mode().Perm()),
	}
	populateVolumeEntryPlatformStat(info, &stat)
	if rel == "" {
		stat.Name = "/"
		stat.Path = "/"
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		if _, err := root.Stat(rootPath); err != nil {
			if os.IsNotExist(err) {
				return gateway.VolumeEntryStat{}, errdefs.NotFound(fmt.Errorf("volume path not found"))
			}
			return gateway.VolumeEntryStat{}, wrapVolumeContentPathError("resolve volume symbolic link", err)
		}
		stat.Type = "symlink"
		target, err := root.Readlink(rootPath)
		// 只回显仍位于卷内的目标，避免泄露宿主机绝对路径或卷外路径信息。
		if err == nil && volumeSymlinkTargetWithinRoot(rel, target) {
			stat.Target = target
		}
	case info.IsDir():
		stat.Type = "directory"
	default:
		stat.Type = "file"
	}
	return stat, nil
}

func volumeSymlinkTargetWithinRoot(rel string, target string) bool {
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return false
	}
	normalized := strings.ReplaceAll(target, "\\", "/")
	if pathpkg.IsAbs(normalized) {
		return false
	}
	resolved := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(rel), normalized))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

func collectVolumeDirEntries(root *os.Root, rel string, depth int, result *[]gateway.VolumeEntryStat) error {
	dir, err := root.Open(".")
	if err != nil {
		return wrapVolumeContentPathError("open volume directory", err)
	}
	entries, err := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err != nil {
		return fmt.Errorf("read volume directory: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close volume directory: %w", closeErr)
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), dockerLocalVolumeMetadataFile) {
			continue
		}
		entryRel := pathpkg.Join(rel, entry.Name())
		stat, err := volumeEntryStat(root, filepath.FromSlash(entry.Name()), entryRel)
		if err != nil {
			return err
		}
		*result = append(*result, stat)
		if entry.IsDir() && depth > 1 {
			childRoot, err := root.OpenRoot(filepath.FromSlash(entry.Name()))
			if err != nil {
				return wrapVolumeContentPathError("open volume directory", err)
			}
			err = collectVolumeDirEntries(childRoot, entryRel, depth-1, result)
			closeErr := childRoot.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return fmt.Errorf("close volume directory root: %w", closeErr)
			}
		}
	}
	return nil
}
