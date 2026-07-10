package gateway

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// maxVolumeFileUploadBytes 限制单次流式上传，避免客户端耗尽宿主机磁盘。
var maxVolumeFileUploadBytes int64 = 512 << 20

func (a *App) volumeContentRuntime() (VolumeContentRuntime, error) {
	runtime, ok := a.runtime.(VolumeContentRuntime)
	if !ok {
		return nil, gatewayError(http.StatusNotImplemented, "volume content is not supported by this runtime")
	}
	return runtime, nil
}

func (a *App) handleVolumeContentPathGet(c *gin.Context) {
	runtime, err := a.volumeContentRuntime()
	if err != nil {
		writeGatewayError(c, err, http.StatusNotImplemented)
		return
	}
	stat, err := runtime.GetVolumePathInfo(a.callbackContext(c), c.Param("volumeID"), c.Query("path"))
	if err != nil {
		writeGatewayError(c, err, volumeErrorStatus(err))
		return
	}
	c.JSON(http.StatusOK, stat)
}

func (a *App) handleVolumeContentFileGet(c *gin.Context) {
	runtime, err := a.volumeContentRuntime()
	if err != nil {
		writeGatewayError(c, err, http.StatusNotImplemented)
		return
	}
	body, err := runtime.ReadVolumeFile(a.callbackContext(c), c.Param("volumeID"), c.Query("path"))
	if err != nil {
		writeGatewayError(c, err, volumeErrorStatus(err))
		return
	}
	defer func() { _ = body.Close() }()
	c.Header("Content-Type", "application/octet-stream")
	if _, err := io.Copy(c.Writer, body); err != nil {
		if c.Writer.Written() {
			// 响应头和部分文件已发送，此时追加 JSON 错误体只会进一步污染下载内容。
			a.logger.Printf("volume content download failed volume_id=%s path=%q error=%v", c.Param("volumeID"), c.Query("path"), err)
			return
		}
		writeGatewayError(c, err, http.StatusInternalServerError)
		return
	}
}

func (a *App) handleVolumeContentFilePut(c *gin.Context) {
	runtime, err := a.volumeContentRuntime()
	if err != nil {
		writeGatewayError(c, err, http.StatusNotImplemented)
		return
	}
	opts, err := volumeWriteOptionsFromQuery(c)
	if err != nil {
		writeGatewayError(c, err, http.StatusBadRequest)
		return
	}
	// MaxBytesReader 在流式读取过程中强制上限，底层原子写保证超限时不会发布临时内容。
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxVolumeFileUploadBytes)
	stat, err := runtime.WriteVolumeFile(a.callbackContext(c), c.Param("volumeID"), c.Query("path"), body, opts)
	if err != nil {
		writeVolumeContentError(c, err, volumeErrorStatus(err))
		return
	}
	c.JSON(http.StatusOK, stat)
}

func (a *App) handleVolumeContentDirGet(c *gin.Context) {
	runtime, err := a.volumeContentRuntime()
	if err != nil {
		writeGatewayError(c, err, http.StatusNotImplemented)
		return
	}
	depth := 1
	if value := strings.TrimSpace(c.Query("depth")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeGatewayError(c, gatewayError(http.StatusBadRequest, "depth must be a non-negative integer"), http.StatusBadRequest)
			return
		}
		depth = parsed
	}
	entries, err := runtime.ListVolumeDir(a.callbackContext(c), c.Param("volumeID"), c.Query("path"), depth)
	if err != nil {
		writeGatewayError(c, err, volumeErrorStatus(err))
		return
	}
	c.JSON(http.StatusOK, entries)
}

func (a *App) handleVolumeContentDirPost(c *gin.Context) {
	runtime, err := a.volumeContentRuntime()
	if err != nil {
		writeGatewayError(c, err, http.StatusNotImplemented)
		return
	}
	opts, err := volumeWriteOptionsFromQuery(c)
	if err != nil {
		writeGatewayError(c, err, http.StatusBadRequest)
		return
	}
	stat, err := runtime.CreateVolumeDir(a.callbackContext(c), c.Param("volumeID"), c.Query("path"), opts)
	if err != nil {
		writeGatewayError(c, err, volumeErrorStatus(err))
		return
	}
	c.JSON(http.StatusOK, stat)
}

func volumeWriteOptionsFromQuery(c *gin.Context) (VolumeWriteOptions, error) {
	var opts VolumeWriteOptions
	if value := strings.TrimSpace(c.Query("force")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return opts, gatewayError(http.StatusBadRequest, "force must be a boolean")
		}
		opts.Force = parsed
	}
	if value := strings.TrimSpace(c.Query("mode")); value != "" {
		parsed, err := parseVolumeMode(value)
		if err != nil {
			return opts, err
		}
		opts.Mode = &parsed
	}
	for _, item := range []struct {
		name string
		dst  **int
	}{
		{name: "uid", dst: &opts.UID},
		{name: "gid", dst: &opts.GID},
	} {
		value := strings.TrimSpace(c.Query(item.name))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return opts, gatewayError(http.StatusBadRequest, "%s must be a non-negative integer", item.name)
		}
		*item.dst = &parsed
	}
	return opts, nil
}

func parseVolumeMode(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, gatewayError(http.StatusBadRequest, "mode must be a decimal integer or an octal integer prefixed with 0 or 0o")
	}

	// 普通数字按十进制解析；前导 0 或 0o 前缀明确表示传统 Unix 八进制权限。
	base := 10
	parseValue := value
	if strings.HasPrefix(value, "0o") || strings.HasPrefix(value, "0O") {
		base = 8
		parseValue = value[2:]
	} else if strings.HasPrefix(value, "0") && len(value) > 1 {
		base = 8
	}

	parsed64, err := strconv.ParseInt(parseValue, base, 0)
	if err != nil || parsed64 < 0 {
		return 0, gatewayError(http.StatusBadRequest, "mode must be a decimal integer or an octal integer prefixed with 0 or 0o")
	}
	parsed := int(parsed64)
	if parsed <= 0o777 {
		return parsed, nil
	}
	return 0, gatewayError(http.StatusBadRequest, "mode must be between 0 and 0777")
}

func writeVolumeContentError(c *gin.Context, err error, fallback int) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		// 将流中途触发的大小限制转换为稳定的 413，而不是泛化成内部错误。
		writeGatewayError(c, gatewayError(http.StatusRequestEntityTooLarge, "volume file upload is too large"), http.StatusRequestEntityTooLarge)
		return
	}
	writeGatewayError(c, err, fallback)
}
