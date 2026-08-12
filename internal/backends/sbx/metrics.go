package sbxbackend

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type metricsAPI interface {
	Read(ctx context.Context, containerID string) (map[string]float64, error)
}

type metricsClient struct {
	root string
}

func newMetricsClient(root string) *metricsClient {
	return &metricsClient{root: strings.TrimSpace(root)}
}

func (c *metricsClient) Read(ctx context.Context, containerID string) (map[string]float64, error) {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return nil, fmt.Errorf("container id is required for metrics")
	}
	if c.root == "" {
		return nil, fmt.Errorf("sbx.metrics_root is required for metrics")
	}

	socketPath := filepath.Join(c.root, containerID, "vm", "metrics.sock")
	connection, err := dialLongUnixSocket(ctx, socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect metrics socket: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://metrics/metrics", nil)
	if err != nil {
		return nil, fmt.Errorf("create metrics request: %w", err)
	}
	if err := request.Write(connection); err != nil {
		return nil, fmt.Errorf("write metrics request: %w", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		return nil, fmt.Errorf("read metrics response: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		return nil, fmt.Errorf("metrics returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read metrics payload: %w", err)
	}
	values, err := parsePrometheusMetrics(data)
	if err != nil {
		return nil, fmt.Errorf("parse Prometheus metrics: %w", err)
	}
	return values, nil
}

// Sailor's per-VM metrics socket path can exceed the Unix sockaddr pathname
// limit. A short-lived directory symlink keeps the address short without
// changing this process's working directory.
func dialLongUnixSocket(ctx context.Context, socketPath string) (net.Conn, error) {
	shortDir, err := os.MkdirTemp("/tmp", "e2b-sbx-metrics-")
	if err != nil {
		return nil, fmt.Errorf("create short metrics socket directory: %w", err)
	}
	defer os.RemoveAll(shortDir)

	linkedParent := filepath.Join(shortDir, "socket-parent")
	if err := os.Symlink(filepath.Dir(socketPath), linkedParent); err != nil {
		return nil, fmt.Errorf("link metrics socket directory: %w", err)
	}
	shortSocketPath := filepath.Join(linkedParent, filepath.Base(socketPath))
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", shortSocketPath)
	if err != nil {
		return nil, fmt.Errorf("dial metrics socket: %w", err)
	}
	return connection, nil
}

func parsePrometheusMetrics(payload []byte) (map[string]float64, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("parse Prometheus metric families: %w", err)
	}
	values := map[string]float64{}
	for name, family := range families {
		for _, metric := range family.GetMetric() {
			switch {
			case metric.GetCounter() != nil:
				values[name] += metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				values[name] += metric.GetGauge().GetValue()
			case metric.GetUntyped() != nil:
				values[name] += metric.GetUntyped().GetValue()
			case metric.GetHistogram() != nil:
				values[name] += float64(metric.GetHistogram().GetSampleCount())
			case metric.GetSummary() != nil:
				values[name] += float64(metric.GetSummary().GetSampleCount())
			}
		}
	}
	return values, nil
}

func metricValueBySuffix(values map[string]float64, suffixes ...string) float64 {
	for _, suffix := range suffixes {
		for name, value := range values {
			if strings.HasSuffix(name, suffix) {
				return value
			}
		}
	}
	return 0
}
