package gpu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"clauzette/internal/config"
)

// ErrNoMetrics means GPU memory cannot be read: gpu.metrics_url is not set.
var ErrNoMetrics = errors.New("GPU memory is not available (gpu.metrics_url is not set)")

// Memory reads GPU memory from the exporter on demand, for /adapt and for
// checking adapted values. Unlike the Guard it needs only metrics_url, so it
// works when pausing is disabled. A nil *Memory reports ErrNoMetrics.
type Memory struct {
	cfg config.GPUConfig
	hc  *http.Client
}

// NewMemory returns nil when no metrics endpoint is configured.
func NewMemory(cfg config.GPUConfig) *Memory {
	if cfg.MetricsURL == "" {
		return nil
	}
	if cfg.MemScale <= 0 {
		cfg.MemScale = 1
	}
	return &Memory{cfg: cfg, hc: &http.Client{Timeout: 5 * time.Second}}
}

// Read returns used and total GPU memory in bytes, summed over all GPUs.
func (m *Memory) Read(ctx context.Context) (used, total int64, err error) {
	if m == nil {
		return 0, 0, ErrNoMetrics
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.MetricsURL, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("metrics endpoint returned %s", resp.Status)
	}
	var u, t, f float64
	var hasU, hasT, hasF bool
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 8<<20))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, val, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		switch name {
		case m.cfg.MemUsedMetric:
			u, hasU = u+val, true
		case m.cfg.MemTotalMetric:
			t, hasT = t+val, true
		case m.cfg.MemFreeMetric:
			f, hasF = f+val, true
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if !hasU || (!hasT && !hasF) {
		return 0, 0, fmt.Errorf("GPU memory metrics not found at %s (gpu.mem_used_metric %q with mem_total_metric %q or mem_free_metric %q)",
			m.cfg.MetricsURL, m.cfg.MemUsedMetric, m.cfg.MemTotalMetric, m.cfg.MemFreeMetric)
	}
	if !hasT {
		t = u + f
	}
	s := m.cfg.MemScale
	return int64(u * s), int64(t * s), nil
}
