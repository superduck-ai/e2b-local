package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

const (
	maxTemplateFileUploadBytes          int64 = 512 << 20
	maxTemplateArchiveEntries                 = 10000
	maxTemplateArchiveUncompressedBytes int64 = 512 << 20
)

func MaxTemplateArchiveEntries() int {
	return maxTemplateArchiveEntries
}

func MaxTemplateArchiveUncompressedBytes() int64 {
	return maxTemplateArchiveUncompressedBytes
}

type TemplateBuildFile struct {
	TemplateID string
	Hash       string
	Data       []byte
	Path       string
	Size       int64
	SHA256     string
}

func (file TemplateBuildFile) Open() (io.ReadCloser, error) {
	if path := strings.TrimSpace(file.Path); path != "" {
		return os.Open(path)
	}
	return io.NopCloser(bytes.NewReader(file.Data)), nil
}

func (file TemplateBuildFile) ReadAll() ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func SafeTemplateBuildContextTarName(name string) (string, bool) {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", false
		}
	}
	name = path.Clean(name)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

func ValidateTemplateBuildFileArchive(file TemplateBuildFile) error {
	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("open uploaded template file archive %s: %w", file.Hash, err)
	}
	defer reader.Close()
	return validateTemplateArchive(reader, file.Hash)
}

func validateTemplateArchive(reader io.Reader, hash string) error {
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return gatewayError(400, "uploaded template file archive %s must be a gzip tar archive: %s", hash, err.Error())
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	entryCount := 0
	var uncompressedBytes int64
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return gatewayError(400, "read uploaded template file archive %s: %s", hash, err.Error())
		}

		entryCount++
		if entryCount > maxTemplateArchiveEntries {
			return gatewayError(413, "uploaded template file archive %s contains more than %d entries", hash, maxTemplateArchiveEntries)
		}
		if _, ok := SafeTemplateBuildContextTarName(header.Name); !ok {
			return gatewayError(400, "uploaded template file archive %s contains unsafe path %q", hash, header.Name)
		}
		if header.Linkname != "" {
			if _, ok := SafeTemplateBuildContextTarName(header.Linkname); !ok {
				return gatewayError(400, "uploaded template file archive %s contains unsafe link path %q", hash, header.Linkname)
			}
		}

		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if header.Size < 0 {
			return gatewayError(400, "uploaded template file archive %s contains invalid size for %q", hash, header.Name)
		}
		if header.Size > maxTemplateArchiveUncompressedBytes-uncompressedBytes {
			return gatewayError(413, "uploaded template file archive %s exceeds %d uncompressed bytes", hash, maxTemplateArchiveUncompressedBytes)
		}
		n, err := io.Copy(io.Discard, tarReader)
		if err != nil {
			return gatewayError(400, "read uploaded template file archive %s entry %q: %s", hash, header.Name, err.Error())
		}
		uncompressedBytes += n
		if uncompressedBytes > maxTemplateArchiveUncompressedBytes {
			return gatewayError(413, "uploaded template file archive %s exceeds %d uncompressed bytes", hash, maxTemplateArchiveUncompressedBytes)
		}
	}
}

func writeTemplateUploadTemp(reader io.Reader) (string, int64, string, error) {
	tempFile, err := os.CreateTemp("", "e2b-template-upload-*.tar.gz")
	if err != nil {
		return "", 0, "", fmt.Errorf("create template upload temp file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		_ = tempFile.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tempFile, hasher), io.LimitReader(reader, maxTemplateFileUploadBytes+1))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return "", written, "", gatewayError(http.StatusRequestEntityTooLarge, "template file upload is too large")
		}
		return "", written, "", err
	}
	if written > maxTemplateFileUploadBytes {
		return "", written, "", gatewayError(http.StatusRequestEntityTooLarge, "template file upload is too large")
	}
	if written == 0 {
		return "", 0, "", gatewayError(http.StatusBadRequest, "template file upload body is required")
	}
	if err := tempFile.Close(); err != nil {
		return "", written, "", fmt.Errorf("close template upload temp file: %w", err)
	}
	cleanup = false
	return tempPath, written, hex.EncodeToString(hasher.Sum(nil)), nil
}

func expectedTemplateUploadSHA256(hash string) (string, bool) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	hash = strings.TrimPrefix(hash, "sha256:")
	if len(hash) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", false
	}
	return hash, true
}

func validateTemplateUploadHash(cacheHash string, actualSHA256 string) error {
	expected, ok := expectedTemplateUploadSHA256(cacheHash)
	if !ok {
		return nil
	}
	if !strings.EqualFold(expected, actualSHA256) {
		return gatewayError(400, "template file upload hash mismatch")
	}
	return nil
}
