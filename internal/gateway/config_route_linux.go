//go:build linux

package gateway

import (
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
)

func detectRouteOutboundHost(probeAddr string) (string, error) {
	targetIP, err := routeProbeIP(probeAddr)
	if err != nil {
		return "", err
	}

	routes, err := netlink.RouteGet(targetIP)
	if err != nil {
		return "", fmt.Errorf("netlink route get %s: %w", targetIP.String(), err)
	}
	for _, route := range routes {
		if isUsableAdvertisedIP(route.Src) {
			return route.Src.To4().String(), nil
		}
		if route.LinkIndex <= 0 {
			continue
		}
		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			continue
		}
		if host, err := DetectInterfaceHost(link.Attrs().Name); err == nil {
			return host, nil
		}
	}

	return "", fmt.Errorf("netlink route get %s did not return a usable IPv4 source address", targetIP.String())
}

func routeProbeIP(probeAddr string) (net.IP, error) {
	probeAddr = strings.TrimSpace(probeAddr)
	if probeAddr == "" {
		return nil, fmt.Errorf("traffic.advertised_probe_addr is required when traffic.advertised_host is empty")
	}

	host, _, err := net.SplitHostPort(probeAddr)
	if err != nil {
		return nil, fmt.Errorf("traffic.advertised_probe_addr must be host:port: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve traffic.advertised_probe_addr host %q: %w", host, err)
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("resolve traffic.advertised_probe_addr host %q: no IPv4 address", host)
}
