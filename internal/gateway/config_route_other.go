//go:build !linux

package gateway

func detectRouteOutboundHost(probeAddr string) (string, error) {
	return "", errRouteDetectionUnsupported
}
