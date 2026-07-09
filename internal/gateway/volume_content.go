package gateway

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

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
		writeGatewayError(c, err, http.StatusNotFound)
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
		writeGatewayError(c, err, http.StatusNotFound)
		return
	}
	defer body.Close()
	c.Header("Content-Type", "application/octet-stream")
	if _, err := io.Copy(c.Writer, body); err != nil {
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
	stat, err := runtime.WriteVolumeFile(a.callbackContext(c), c.Param("volumeID"), c.Query("path"), c.Request.Body, opts)
	if err != nil {
		writeGatewayError(c, err, http.StatusInternalServerError)
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
		writeGatewayError(c, err, http.StatusNotFound)
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
		writeGatewayError(c, err, http.StatusInternalServerError)
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
	for _, item := range []struct {
		name string
		dst  **int
	}{
		{name: "mode", dst: &opts.Mode},
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
