// Package gpu watches GPU metrics from a Prometheus-format exporter (DCGM
// exporter, or an nvidia-smi based exporter) and lets the agent pause
// between model calls to let the GPU cool down.
package gpu

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"clauzette/internal/config"
)

type sample struct {
	at      time.Time
	temp    float64
	util    float64
	hasTemp bool
	hasUtil bool
}

// Guard is safe to use as a nil pointer: every method then does nothing.
type Guard struct {
	cfg     config.GPUConfig
	hc      *http.Client
	mu      sync.Mutex
	samples []sample
	lastErr error
	skip    chan struct{}
}

// New returns nil when the guard is disabled.
func New(cfg config.GPUConfig) *Guard {
	if !cfg.Enabled || cfg.MetricsURL == "" {
		return nil
	}
	if cfg.SampleSec <= 0 {
		cfg.SampleSec = 5
	}
	if cfg.UtilScale == 0 {
		cfg.UtilScale = 1
	}
	return &Guard{cfg: cfg, hc: &http.Client{Timeout: 5 * time.Second}, skip: make(chan struct{}, 1)}
}

func (g *Guard) interval() time.Duration { return time.Duration(g.cfg.SampleSec) * time.Second }

// Start samples the metrics endpoint in the background until ctx ends.
func (g *Guard) Start(ctx context.Context) {
	if g == nil {
		return
	}
	go func() {
		g.sampleOnce(ctx)
		t := time.NewTicker(g.interval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.sampleOnce(ctx)
			}
		}
	}()
}

func (g *Guard) sampleOnce(ctx context.Context) {
	s, err := g.fetch(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.lastErr = err
		return
	}
	g.lastErr = nil
	g.samples = append(g.samples, s)
	keep := time.Duration(g.cfg.UtilWindowSec)*time.Second + 3*g.interval()
	if keep < 2*time.Minute {
		keep = 2 * time.Minute
	}
	cut := time.Now().Add(-keep)
	i := 0
	for i < len(g.samples) && g.samples[i].at.Before(cut) {
		i++
	}
	g.samples = append([]sample(nil), g.samples[i:]...)
}

func (g *Guard) fetch(ctx context.Context) (sample, error) {
	s := sample{at: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.MetricsURL, nil)
	if err != nil {
		return s, err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("metrics endpoint returned %s", resp.Status)
	}
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
		// With several GPUs, the hottest / busiest one counts.
		switch name {
		case g.cfg.TempMetric:
			if !s.hasTemp || val > s.temp {
				s.temp = val
			}
			s.hasTemp = true
		case g.cfg.UtilMetric:
			v := val * g.cfg.UtilScale
			if !s.hasUtil || v > s.util {
				s.util = v
			}
			s.hasUtil = true
		}
	}
	if err := sc.Err(); err != nil {
		return s, err
	}
	if !s.hasTemp && !s.hasUtil {
		return s, fmt.Errorf("neither %s nor %s found at %s", g.cfg.TempMetric, g.cfg.UtilMetric, g.cfg.MetricsURL)
	}
	return s, nil
}

// parseMetricLine parses `name{labels} value [timestamp]` or `name value`.
func parseMetricLine(line string) (string, float64, bool) {
	var name, rest string
	i := strings.IndexAny(line, "{ \t")
	switch {
	case i <= 0:
		return "", 0, false
	case line[i] == '{':
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", 0, false
		}
		name, rest = line[:i], line[j+1:]
	default:
		name, rest = line[:i], line[i:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", 0, false
	}
	return name, v, true
}

func (g *Guard) latest() (sample, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.samples) == 0 {
		return sample{}, false
	}
	s := g.samples[len(g.samples)-1]
	if time.Since(s.at) > 3*g.interval() {
		return s, false // stale
	}
	return s, true
}

func (g *Guard) clearSamples() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.samples = nil
}

// utilSaturated reports whether every sample in the window is above the
// threshold and the samples actually cover the window.
func (g *Guard) utilSaturated() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	window := time.Duration(g.cfg.UtilWindowSec) * time.Second
	from := time.Now().Add(-window)
	n := 0
	var earliest time.Time
	for _, s := range g.samples {
		if s.at.Before(from) {
			continue
		}
		if !s.hasUtil || s.util < g.cfg.UtilThreshold {
			return false
		}
		if n == 0 {
			earliest = s.at
		}
		n++
	}
	return n >= 2 && earliest.Sub(from) <= 2*g.interval()
}

func (g *Guard) check() (kind, reason string) {
	mode := g.cfg.Mode
	if mode == "temperature" || mode == "both" {
		if s, ok := g.latest(); ok && s.hasTemp && s.temp >= g.cfg.PauseAboveC {
			return "temp", fmt.Sprintf("temperature %.0f°C ≥ %.0f°C", s.temp, g.cfg.PauseAboveC)
		}
	}
	if mode == "utilization" || mode == "both" {
		if g.utilSaturated() {
			return "util", fmt.Sprintf("utilization ≥ %.0f%% for the last %ds", g.cfg.UtilThreshold, g.cfg.UtilWindowSec)
		}
	}
	return "", ""
}

// Wait returns immediately unless a cool-down rule triggers. Then it blocks
// until the GPU has cooled down (temperature rule), the pause time has
// passed (utilization rule), Skip is called, the maximum pause is reached,
// or ctx is cancelled.
func (g *Guard) Wait(ctx context.Context, notify func(string)) error {
	if g == nil {
		return nil
	}
	select { // forget a !skip typed while no pause was active
	case <-g.skip:
	default:
	}
	kind, reason := g.check()
	if kind == "" {
		return nil
	}
	notify(fmt.Sprintf("GPU cool-down: %s; pausing before the next model request (!skip to continue, !c to cancel)", reason))
	start := time.Now()
	maxPause := time.Duration(g.cfg.MaxPauseSec) * time.Second
	utilPause := time.Duration(g.cfg.UtilPauseSec) * time.Second
	t := time.NewTicker(g.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.skip:
			g.clearSamples()
			notify("cool-down skipped")
			return nil
		case <-t.C:
		}
		elapsed := time.Since(start).Round(time.Second)
		if maxPause > 0 && elapsed >= maxPause {
			notify(fmt.Sprintf("cool-down: maximum pause of %s reached, continuing", maxPause))
			return nil
		}
		switch kind {
		case "temp":
			if s, ok := g.latest(); ok && s.hasTemp && s.temp < g.cfg.ResumeBelowC {
				notify(fmt.Sprintf("cool-down finished after %s (%.0f°C)", elapsed, s.temp))
				return nil
			}
		case "util":
			if elapsed >= utilPause {
				g.clearSamples() // start a fresh window
				notify(fmt.Sprintf("cool-down finished after %s", elapsed))
				return nil
			}
		}
	}
}

// Skip ends an active cool-down pause.
func (g *Guard) Skip() {
	if g == nil {
		return
	}
	select {
	case g.skip <- struct{}{}:
	default:
	}
}

// Status is a short label for the prompt line, e.g. "GPU 67°C 98%".
func (g *Guard) Status() string {
	if g == nil {
		return ""
	}
	s, ok := g.latest()
	if !ok {
		return "GPU n/a"
	}
	parts := []string{"GPU"}
	if s.hasTemp {
		parts = append(parts, fmt.Sprintf("%.0f°C", s.temp))
	}
	if s.hasUtil {
		parts = append(parts, fmt.Sprintf("%.0f%%", s.util))
	}
	return strings.Join(parts, " ")
}

// Report describes the guard's state for the /gpu command.
func (g *Guard) Report() string {
	if g == nil {
		return "GPU guard is disabled (gpu.enabled is false)."
	}
	g.mu.Lock()
	lastErr, n := g.lastErr, len(g.samples)
	g.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Metrics: %s (%d recent samples)\n", g.cfg.MetricsURL, n)
	fmt.Fprintf(&sb, "Current: %s\n", g.Status())
	switch g.cfg.Mode {
	case "temperature":
		fmt.Fprintf(&sb, "Rule: pause above %.0f°C, resume below %.0f°C\n", g.cfg.PauseAboveC, g.cfg.ResumeBelowC)
	case "utilization":
		fmt.Fprintf(&sb, "Rule: pause %ds after %ds at ≥ %.0f%% utilization\n", g.cfg.UtilPauseSec, g.cfg.UtilWindowSec, g.cfg.UtilThreshold)
	default:
		fmt.Fprintf(&sb, "Rules: temperature (%.0f/%.0f°C) and utilization (≥ %.0f%% for %ds → %ds pause)\n",
			g.cfg.PauseAboveC, g.cfg.ResumeBelowC, g.cfg.UtilThreshold, g.cfg.UtilWindowSec, g.cfg.UtilPauseSec)
	}
	if lastErr != nil {
		fmt.Fprintf(&sb, "Last error: %v\n", lastErr)
	}
	return sb.String()
}
