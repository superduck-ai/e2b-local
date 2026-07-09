package dockerbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gateway "e2b-local/internal/gateway"

	"github.com/docker/docker/errdefs"
)

type dockerLocalVolumeMetadata struct {
	VolumeID  string    `json:"volume_id"`
	Name      string    `json:"name"`
	Managed   bool      `json:"managed"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *DockerRuntime) CreateVolume(_ context.Context, name string) (RuntimeVolume, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return RuntimeVolume{}, fmt.Errorf("volume name is required")
	}
	dir, err := r.localVolumeHostDir(name)
	if err != nil {
		return RuntimeVolume{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return RuntimeVolume{}, fmt.Errorf("create docker local volume root: %w", err)
	}
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return RuntimeVolume{}, fmt.Errorf("docker local volume path %s is not a directory", dir)
		}
		volume, err := readDockerLocalVolumeMetadata(dir)
		if err != nil {
			return RuntimeVolume{}, fmt.Errorf("refusing to use unmanaged docker local volume directory %s", dir)
		}
		if volume.VolumeID != name {
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
		VolumeID:  name,
		Name:      name,
		Managed:   true,
		CreatedAt: time.Now().UTC(),
	}
	if err := writeDockerLocalVolumeMetadata(dir, metadata); err != nil {
		return RuntimeVolume{}, err
	}
	return RuntimeVolume{VolumeID: name, Name: name}, nil
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
		volume, err := readDockerLocalVolumeMetadata(filepath.Join(base, entry.Name()))
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

func (r *DockerRuntime) DeleteVolume(_ context.Context, volumeID string) (bool, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("remove docker local volume %s: %w", volumeID, err)
	}
	return true, nil
}

func (r *DockerRuntime) GetVolumePathInfo(_ context.Context, volumeID string, path string) (gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	rel, fullPath, err := r.resolveVolumeContentPath(dir, path, true, true)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	return volumeEntryStat(fullPath, rel)
}

func (r *DockerRuntime) ReadVolumeFile(_ context.Context, volumeID string, path string) (io.ReadCloser, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return nil, err
	}
	_, fullPath, err := r.resolveVolumeContentPath(dir, path, false, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFound(fmt.Errorf("volume file not found"))
		}
		return nil, fmt.Errorf("stat volume file: %w", err)
	}
	if info.IsDir() {
		return nil, gateway.NewGatewayError(http.StatusBadRequest, "path is a directory")
	}
	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFound(fmt.Errorf("volume file not found"))
		}
		return nil, fmt.Errorf("read volume file: %w", err)
	}
	return file, nil
}

func (r *DockerRuntime) WriteVolumeFile(_ context.Context, volumeID string, path string, body io.Reader, opts gateway.VolumeWriteOptions) (gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	rel, fullPath, err := r.resolveVolumeContentPath(dir, path, false, false)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	parent := filepath.Dir(fullPath)
	if err := ensureVolumeDirForCreate(dir, parent, 0o755); err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	if info, err := os.Lstat(fullPath); err == nil {
		if info.IsDir() {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusBadRequest, "path is a directory")
		}
		if !opts.Force {
			return gateway.VolumeEntryStat{}, gateway.NewGatewayError(http.StatusConflict, "path already exists")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := ensurePathWithinRoot(dir, fullPath); err != nil {
				return gateway.VolumeEntryStat{}, err
			}
		}
	} else if !os.IsNotExist(err) {
		return gateway.VolumeEntryStat{}, fmt.Errorf("stat volume file: %w", err)
	}
	mode := os.FileMode(0o644)
	if opts.Mode != nil {
		mode = os.FileMode(*opts.Mode)
	}
	file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return gateway.VolumeEntryStat{}, fmt.Errorf("write volume file: %w", err)
	}
	_, copyErr := io.Copy(file, body)
	closeErr := file.Close()
	if copyErr != nil {
		return gateway.VolumeEntryStat{}, fmt.Errorf("write volume file: %w", copyErr)
	}
	if closeErr != nil {
		return gateway.VolumeEntryStat{}, fmt.Errorf("close volume file: %w", closeErr)
	}
	if opts.Mode != nil {
		if err := os.Chmod(fullPath, os.FileMode(*opts.Mode)); err != nil {
			return gateway.VolumeEntryStat{}, fmt.Errorf("chmod volume file: %w", err)
		}
	}
	if opts.UID != nil || opts.GID != nil {
		uid := -1
		gid := -1
		if opts.UID != nil {
			uid = *opts.UID
		}
		if opts.GID != nil {
			gid = *opts.GID
		}
		if err := os.Chown(fullPath, uid, gid); err != nil {
			return gateway.VolumeEntryStat{}, fmt.Errorf("chown volume file: %w", err)
		}
	}
	return volumeEntryStat(fullPath, rel)
}

func (r *DockerRuntime) ListVolumeDir(_ context.Context, volumeID string, path string, depth int) ([]gateway.VolumeEntryStat, error) {
	_, dir, err := r.localVolume(volumeID)
	if err != nil {
		return nil, err
	}
	_, fullPath, err := r.resolveVolumeContentPath(dir, path, true, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFound(fmt.Errorf("volume directory not found"))
		}
		return nil, fmt.Errorf("stat volume directory: %w", err)
	}
	if !info.IsDir() {
		return nil, gateway.NewGatewayError(http.StatusBadRequest, "path is not a directory")
	}
	if depth <= 0 {
		depth = 1
	}
	var result []gateway.VolumeEntryStat
	if err := collectVolumeDirEntries(dir, fullPath, depth, &result); err != nil {
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
	rel, fullPath, err := r.resolveVolumeContentPath(dir, path, true, false)
	if err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	mode := os.FileMode(0o755)
	if opts.Mode != nil {
		mode = os.FileMode(*opts.Mode)
	}
	if err := ensureVolumeDirForCreate(dir, fullPath, mode); err != nil {
		return gateway.VolumeEntryStat{}, err
	}
	if opts.Mode != nil {
		if err := os.Chmod(fullPath, mode); err != nil {
			return gateway.VolumeEntryStat{}, fmt.Errorf("chmod volume directory: %w", err)
		}
	}
	return volumeEntryStat(fullPath, rel)
}

func (r *DockerRuntime) localVolume(volumeID string) (RuntimeVolume, string, error) {
	dir, err := r.localVolumeHostDir(volumeID)
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
	volume, err := readDockerLocalVolumeMetadata(dir)
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
	root, err := r.ensureLocalVolumeRoot()
	if err != nil {
		return "", err
	}
	name, err := dockerLocalVolumeDirName(volumeID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if err := ensurePathLexicallyWithinRoot(root, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func dockerLocalVolumeDirName(volumeID string) (string, error) {
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return "", fmt.Errorf("volume id is required")
	}
	if strings.Contains(volumeID, "\x00") || strings.ContainsAny(volumeID, `/\`) || volumeID == "." || volumeID == ".." {
		return "", gateway.NewGatewayError(http.StatusBadRequest, "invalid volume id")
	}
	return volumeID, nil
}

func readDockerLocalVolumeMetadata(dir string) (RuntimeVolume, error) {
	data, err := os.ReadFile(filepath.Join(dir, dockerLocalVolumeMetadataFile))
	if err != nil {
		return RuntimeVolume{}, err
	}
	var metadata dockerLocalVolumeMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return RuntimeVolume{}, fmt.Errorf("decode docker local volume metadata: %w", err)
	}
	if !metadata.Managed || strings.TrimSpace(metadata.VolumeID) == "" || strings.TrimSpace(metadata.Name) == "" {
		return RuntimeVolume{}, fmt.Errorf("invalid docker local volume metadata")
	}
	return RuntimeVolume{VolumeID: strings.TrimSpace(metadata.VolumeID), Name: strings.TrimSpace(metadata.Name)}, nil
}

func writeDockerLocalVolumeMetadata(dir string, metadata dockerLocalVolumeMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, dockerLocalVolumeMetadataFile), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write docker local volume metadata: %w", err)
	}
	return nil
}

func (r *DockerRuntime) resolveVolumeContentPath(volumeRoot string, rawPath string, allowRoot bool, mustExist bool) (string, string, error) {
	rel, err := cleanVolumeContentPath(rawPath, allowRoot)
	if err != nil {
		return "", "", err
	}
	fullPath := filepath.Join(volumeRoot, filepath.FromSlash(rel))
	if err := ensurePathLexicallyWithinRoot(volumeRoot, fullPath); err != nil {
		return "", "", err
	}
	if mustExist {
		if err := ensurePathWithinRoot(volumeRoot, fullPath); err != nil {
			return "", "", err
		}
	}
	return rel, fullPath, nil
}

func cleanVolumeContentPath(rawPath string, allowRoot bool) (string, error) {
	rawPath = strings.TrimSpace(rawPath)
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

func ensurePathWithinRoot(root string, target string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve volume root: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return errdefs.NotFound(fmt.Errorf("volume path not found"))
		}
		return fmt.Errorf("resolve volume path: %w", err)
	}
	if err := ensurePathLexicallyWithinRoot(realRoot, realTarget); err != nil {
		return gateway.NewGatewayError(http.StatusBadRequest, "path escapes volume root")
	}
	return nil
}

func ensureVolumeDirForCreate(root string, dir string, mode os.FileMode) error {
	if err := ensurePathLexicallyWithinRoot(root, dir); err != nil {
		return err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			if err := ensurePathWithinRoot(root, current); err != nil {
				return err
			}
			if err := os.MkdirAll(dir, mode); err != nil {
				return fmt.Errorf("create volume directory: %w", err)
			}
			return ensurePathWithinRoot(root, dir)
		}
		if err != nil {
			return fmt.Errorf("stat volume directory component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := ensurePathWithinRoot(root, next); err != nil {
				return err
			}
		} else if !info.IsDir() {
			return gateway.NewGatewayError(http.StatusBadRequest, "path parent is not a directory")
		}
		current = next
	}
	return ensurePathWithinRoot(root, dir)
}

func volumeEntryStat(fullPath string, rel string) (gateway.VolumeEntryStat, error) {
	info, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return gateway.VolumeEntryStat{}, errdefs.NotFound(fmt.Errorf("volume path not found"))
		}
		return gateway.VolumeEntryStat{}, fmt.Errorf("stat volume path: %w", err)
	}
	mtime := info.ModTime()
	stat := gateway.VolumeEntryStat{
		Atime: mtime,
		Mtime: mtime,
		Ctime: mtime,
		Type:  "unknown",
		Name:  filepath.Base(fullPath),
		Path:  "/" + filepath.ToSlash(rel),
		Size:  info.Size(),
		Mode:  int(info.Mode().Perm()),
	}
	if rel == "" {
		stat.Name = "/"
		stat.Path = "/"
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		stat.Type = "symlink"
		target, err := os.Readlink(fullPath)
		if err == nil {
			stat.Target = target
		}
	case info.IsDir():
		stat.Type = "directory"
	default:
		stat.Type = "file"
	}
	return stat, nil
}

func collectVolumeDirEntries(root string, dir string, depth int, result *[]gateway.VolumeEntryStat) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read volume directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == dockerLocalVolumeMetadataFile {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		if err := ensurePathWithinRoot(root, fullPath); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		stat, err := volumeEntryStat(fullPath, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		*result = append(*result, stat)
		if entry.IsDir() && depth > 1 {
			if err := collectVolumeDirEntries(root, fullPath, depth-1, result); err != nil {
				return err
			}
		}
	}
	return nil
}
