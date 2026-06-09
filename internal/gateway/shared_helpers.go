package gateway

import (
	"context"
	"net/http"
	"strings"

	"e2b-local/internal/e2bapi"
)

type inboundHeadersContextKey struct{}
type inboundRequestBaseContextKey struct{}

type inboundRequestBase struct {
	Scheme string
	Host   string
}

func contextWithInboundHeaders(ctx context.Context, headers http.Header) context.Context {
	return context.WithValue(ctx, inboundHeadersContextKey{}, headers.Clone())
}

func contextWithInboundRequest(ctx context.Context, req *http.Request) context.Context {
	scheme := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if req.URL != nil && req.URL.Scheme != "" {
			scheme = req.URL.Scheme
		} else if req.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}

	host := strings.TrimSpace(req.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = req.Host
	}

	return context.WithValue(ctx, inboundRequestBaseContextKey{}, inboundRequestBase{
		Scheme: scheme,
		Host:   host,
	})
}

func inboundHeaders(ctx context.Context) http.Header {
	headers, _ := ctx.Value(inboundHeadersContextKey{}).(http.Header)
	return headers
}

func InboundHeaders(ctx context.Context) http.Header {
	return inboundHeaders(ctx)
}

func inboundRequestBaseURL(ctx context.Context) (string, bool) {
	base, ok := ctx.Value(inboundRequestBaseContextKey{}).(inboundRequestBase)
	if !ok || strings.TrimSpace(base.Scheme) == "" || strings.TrimSpace(base.Host) == "" {
		return "", false
	}
	return base.Scheme + "://" + base.Host, true
}

func InboundRequestBaseURL(ctx context.Context) (string, bool) {
	return inboundRequestBaseURL(ctx)
}

func normalizeVolumeMounts(volumeMounts []VolumeMount) []VolumeMount {
	result := make([]VolumeMount, 0, len(volumeMounts))
	for _, mount := range volumeMounts {
		name := strings.TrimSpace(mount.Name)
		path := strings.TrimSpace(mount.Path)
		volumeID := strings.TrimSpace(mount.VolumeID)
		mountPath := strings.TrimSpace(mount.MountPath)
		if path == "" {
			path = mountPath
		}
		if volumeID == "" {
			volumeID = name
		}
		result = append(result, VolumeMount{
			Name:      name,
			Path:      path,
			VolumeID:  volumeID,
			MountPath: mountPath,
		})
	}
	return result
}

func templateBuildSteps(steps *[]e2bapi.TemplateStep) []e2bapi.TemplateStep {
	if steps == nil {
		return nil
	}
	return append([]e2bapi.TemplateStep(nil), (*steps)...)
}

func shortDockerImageName(imageRef string) string {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return ""
	}

	lastSlash := strings.LastIndex(imageRef, "/")
	return imageRef[lastSlash+1:]
}

func dockerTemplateName(imageRef string) string {
	name := shortDockerImageName(imageRef)
	if digestIndex := strings.Index(name, "@"); digestIndex >= 0 {
		return name[:digestIndex]
	}
	if tagIndex := strings.LastIndex(name, ":"); tagIndex >= 0 {
		return name[:tagIndex]
	}
	return name
}

func isDockerImageReference(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if strings.Contains(ref, "@") {
		return true
	}
	lastSlash := strings.LastIndex(ref, "/")
	return strings.Contains(ref[lastSlash+1:], ":")
}
