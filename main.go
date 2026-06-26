package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
	"gopkg.in/yaml.v3"
)

// ==================== Config ====================

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Defaults DefaultConfig  `yaml:"defaults"`
	Targets  []TargetConfig `yaml:"targets"`
}

type ServerConfig struct {
	Port                 int    `yaml:"port"`
	MetricsPath          string `yaml:"metrics_path"`
	MaxConcurrent        int    `yaml:"max_concurrent"`
	ExposeRuntimeMetrics *bool `yaml:"expose_runtime_metrics"` // expose Go/Process metrics (default true)
}

type DefaultConfig struct {
	Interval       string            `yaml:"interval"`        // HTTP timeout for URL checks
	Timeout        string            `yaml:"timeout"`         // same as Interval, kept for backward compatibility
	ScrapeInterval string            `yaml:"scrape_interval"` // how often to run the background scraper
	GlobalLabels   map[string]string `yaml:"global_labels"`   // global labels injected into all metrics
}

type TargetConfig struct {
	Name           string            `yaml:"name"`
	Type           string            `yaml:"type"`
	URL            string            `yaml:"url"`
	Method         string            `yaml:"method"`
	ExpectedStatus int               `yaml:"expected_status"`
	ExpectedBody   string            `yaml:"expected_body_contains"`
	Headers        map[string]string `yaml:"headers"`
	Mode           string            `yaml:"mode"`
	Names          []string          `yaml:"names"`
	Interval       string            `yaml:"interval"`
	Script         string            `yaml:"script"`
	Labels         map[string]string `yaml:"labels"`
	MaxBodySize    int               `yaml:"max_body_size"` // maximum bytes to read from response body (0 = unlimited)
	Remote         *RemoteTarget     `yaml:"remote"`        // remote metrics endpoint (type=remote)
}

type RemoteTarget struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// Apply defaults
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if portEnv := os.Getenv("EXPORTER_PORT"); portEnv != "" {
		if p, err := strconv.Atoi(portEnv); err == nil && p > 0 {
			cfg.Server.Port = p
		}
	}
	if cfg.Server.MetricsPath == "" {
		cfg.Server.MetricsPath = "/metrics"
	}
	if cfg.Server.MaxConcurrent == 0 {
		cfg.Server.MaxConcurrent = 10
	}

	if cfg.Defaults.Interval == "" {
		cfg.Defaults.Interval = "10s"
	}
	if cfg.Defaults.Timeout == "" {
		cfg.Defaults.Timeout = cfg.Defaults.Interval
	}
	if cfg.Defaults.ScrapeInterval == "" {
		cfg.Defaults.ScrapeInterval = "5s"
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateConfig(cfg *Config) error {
	for i, t := range cfg.Targets {
		if t.Name == "" {
			return fmt.Errorf("target %d: missing 'name'", i)
		}
		switch t.Type {
		case "url":
			if t.URL == "" {
				return fmt.Errorf("target %s: missing 'url'", t.Name)
			}
		case "docker":
			// no mandatory fields beyond name
		case "custom":
			if t.Script == "" {
				return fmt.Errorf("target %s: missing 'script'", t.Name)
			}
		case "remote":
			if t.Remote == nil || t.Remote.URL == "" {
				return fmt.Errorf("target %s: missing 'remote.url'", t.Name)
			}
		default:
			return fmt.Errorf("target %s: unknown type %q", t.Name, t.Type)
		}
	}
	return nil
}

func parseDuration(s string, fallback string) time.Duration {
	if s == "" {
		s = fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		d, _ = time.ParseDuration(fallback)
	}
	return d
}

// extractGlobalLabels returns sorted keys and values map from global labels.
func extractGlobalLabels(labels map[string]string) ([]string, map[string]string) {
	if len(labels) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, labels
}


// ==================== Shared Metrics ====================

type URLMetrics struct {
	up           *prometheus.GaugeVec
	statusCode   *prometheus.GaugeVec
	duration     *prometheus.GaugeVec
	bodyMatch    *prometheus.GaugeVec
	statusMatch  *prometheus.GaugeVec
	responseSize *prometheus.GaugeVec
}

func NewURLMetrics(globalKeys []string) *URLMetrics {
	baseLabels := []string{"name", "env", "tier"}
	allLabels := append(baseLabels, globalKeys...)
	return &URLMetrics{
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_up", Help: "URL probe success (1=yes, 0=no)",
		}, allLabels),
		statusCode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_http_status", Help: "HTTP response status code",
		}, allLabels),
		duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_duration_seconds", Help: "HTTP probe duration",
		}, allLabels),
		bodyMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_body_match", Help: "Expected body content found (1=yes, 0=no)",
		}, allLabels),
		statusMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_status_match", Help: "Expected status code matched (1=yes, 0=no)",
		}, allLabels),
		responseSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_response_size_bytes", Help: "Response body size",
		}, allLabels),
	}
}

type DockerMetrics struct {
	up         *prometheus.GaugeVec
	cpuPercent *prometheus.GaugeVec
	memUsage   *prometheus.GaugeVec
	memLimit   *prometheus.GaugeVec
	diskRw     *prometheus.GaugeVec
	diskRootFs *prometheus.GaugeVec
}

func NewDockerMetrics(globalKeys []string) *DockerMetrics {
	baseLabels := []string{"container", "image", "env", "tier"}
	allLabels := append(baseLabels, globalKeys...)
	return &DockerMetrics{
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_up", Help: "Container is running (1=yes, 0=no)",
		}, allLabels),
		cpuPercent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_cpu_percent", Help: "Container CPU usage percent",
		}, allLabels),
		memUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_memory_usage_bytes", Help: "Container memory usage",
		}, allLabels),
		memLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_memory_limit_bytes", Help: "Container memory limit",
		}, allLabels),
		diskRw: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_disk_rw_bytes", Help: "Container writable layer size",
		}, allLabels),
		diskRootFs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_disk_rootfs_bytes", Help: "Container root filesystem total size",
		}, allLabels),
	}
}

// ==================== Custom Metric Registry (global) ====================

type CustomMetricRegistry struct {
	mu           sync.Mutex
	gauges       map[string]*prometheus.GaugeVec
	reg          *prometheus.Registry
	seenNames    map[string]bool     // track metric names seen in current round
	globalKeys   []string
	globalValues map[string]string
}

func NewCustomMetricRegistry(reg *prometheus.Registry, globalKeys []string, globalValues map[string]string) *CustomMetricRegistry {
	return &CustomMetricRegistry{
		gauges:       make(map[string]*prometheus.GaugeVec),
		reg:          reg,
		seenNames:    make(map[string]bool),
		globalKeys:   globalKeys,
		globalValues: globalValues,
	}
}

// GetOrCreateGauge returns a GaugeVec for the given metric name, creating it if needed.
// All instances share the same GaugeVec, differentiated by labels.
func (r *CustomMetricRegistry) GetOrCreateGauge(name string) *prometheus.GaugeVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		r.seenNames[name] = true
		return g
	}
	labels := append([]string{"name", "env", "tier"}, r.globalKeys...)
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: name,
		Help: "Custom metric from script",
	}, labels)
	r.reg.MustRegister(g)
	r.gauges[name] = g
	r.seenNames[name] = true
	return g
}

// ResetAll resets all custom metrics and clears seen names before a new scrape.
func (r *CustomMetricRegistry) ResetAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.gauges {
		g.Reset()
	}
	r.seenNames = make(map[string]bool)
}

// PurgeStale unregisters GaugeVecs whose names were NOT seen in the current round.
func (r *CustomMetricRegistry) PurgeStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, g := range r.gauges {
		if !r.seenNames[name] {
			r.reg.Unregister(g)
			delete(r.gauges, name)
		}
	}
}

// ==================== URL Collector ====================

type URLCollector struct {
	cfg          TargetConfig
	defaults     DefaultConfig
	metrics      *URLMetrics
	globalValues map[string]string
}

func NewURLCollector(cfg TargetConfig, defaults DefaultConfig, m *URLMetrics, globalValues map[string]string) *URLCollector {
	return &URLCollector{cfg: cfg, defaults: defaults, metrics: m, globalValues: globalValues}
}

func (c *URLCollector) Collect() {
	timeout := parseDuration(c.defaults.Timeout, "10s")
	lv := prometheus.Labels{
		"name": c.cfg.Name,
		"env":  c.cfg.Labels["env"],
		"tier": c.cfg.Labels["tier"],
	}
	for k, v := range c.globalValues {
		lv[k] = v
	}

	method := c.cfg.Method
	if method == "" {
		method = "GET"
	}

	client := &http.Client{Timeout: timeout}
	if c.cfg.ExpectedStatus > 0 {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL, nil)
	if err != nil {
		c.metrics.up.With(lv).Set(0)
		return
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(start).Seconds()

	if err != nil {
		c.metrics.up.With(lv).Set(0)
		c.metrics.duration.With(lv).Set(duration)
		return
	}
	defer resp.Body.Close()

	// Limit body size to prevent memory exhaustion
	maxSize := c.cfg.MaxBodySize
	if maxSize <= 0 {
		maxSize = 10 * 1024 // default 10KB
	}
	limitedReader := io.LimitReader(resp.Body, int64(maxSize)+1) // +1 to detect truncation
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		// still set status and up, but body may be partial
		log.Printf("[url:%s] error reading body: %v", c.cfg.Name, err)
	}
	bodyLen := len(body)
	if bodyLen > maxSize {
		body = body[:maxSize] // truncate for matching
	}

	c.metrics.up.With(lv).Set(1)
	c.metrics.statusCode.With(lv).Set(float64(resp.StatusCode))
	c.metrics.duration.With(lv).Set(duration)
	c.metrics.responseSize.With(lv).Set(float64(bodyLen))

	if c.cfg.ExpectedStatus > 0 {
		match := 0
		if resp.StatusCode == c.cfg.ExpectedStatus {
			match = 1
		}
		c.metrics.statusMatch.With(lv).Set(float64(match))
	}
	if c.cfg.ExpectedBody != "" {
		match := 0
		if strings.Contains(string(body), c.cfg.ExpectedBody) {
			match = 1
		}
		c.metrics.bodyMatch.With(lv).Set(float64(match))
	}
}

// ==================== Docker Collector ====================

type DockerContainer struct {
	ID         string      `json:"Id"`
	Name       string      `json:"Name"`
	State      DockerState `json:"State"`
	Image      string      `json:"Image"`
	SizeRw     int64       `json:"SizeRw"`
	SizeRootFs int64       `json:"SizeRootFs"`
}

type DockerState struct {
	Status string `json:"Status"`
}

type DockerCollector struct {
	cfg          TargetConfig
	metrics      *DockerMetrics
	globalKeys   []string
	globalValues map[string]string
}

func NewDockerCollector(cfg TargetConfig, m *DockerMetrics, globalKeys []string, globalValues map[string]string) *DockerCollector {
	return &DockerCollector{cfg: cfg, metrics: m, globalKeys: globalKeys, globalValues: globalValues}
}

func (c *DockerCollector) Collect() {
	containers, err := c.listContainers()
	if err != nil {
		log.Printf("[docker:%s] list containers: %v", c.cfg.Name, err)
		return
	}

	for _, ct := range containers {
		name := c.cleanName(ct.Name)
		image := ct.Image
		state := ct.State.Status
		isRunning := 0
		if state == "running" {
			isRunning = 1
		}

		lv := []string{name, image, c.cfg.Labels["env"], c.cfg.Labels["tier"]}
		for _, k := range c.globalKeys {
			lv = append(lv, c.globalValues[k])
		}
		c.metrics.up.WithLabelValues(lv...).Set(float64(isRunning))
		c.metrics.diskRw.WithLabelValues(lv...).Set(float64(ct.SizeRw))
		c.metrics.diskRootFs.WithLabelValues(lv...).Set(float64(ct.SizeRootFs))

		if state == "running" {
			stats, err := c.getContainerStats(ct.ID)
			if err == nil && stats != nil {
				c.metrics.cpuPercent.WithLabelValues(lv...).Set(stats.cpuPercent)
				c.metrics.memUsage.WithLabelValues(lv...).Set(stats.memUsage)
				c.metrics.memLimit.WithLabelValues(lv...).Set(stats.memLimit)
			}
		}
	}
}

func (c *DockerCollector) cleanName(name string) string {
	if name == "" {
		return "unknown"
	}
	return strings.TrimPrefix(name, "/")
}

func (c *DockerCollector) listContainers() ([]DockerContainer, error) {
	var containerNames []string
	if c.cfg.Mode == "filter" && len(c.cfg.Names) > 0 {
		containerNames = c.cfg.Names
	} else {
		idCmd := exec.Command("docker", "ps", "-a", "-q")
		ids, err := idCmd.Output()
		if err != nil {
			return nil, err
		}
		containerNames = strings.Fields(strings.TrimSpace(string(ids)))
	}

	if len(containerNames) == 0 {
		return nil, nil
	}

	// Batch inspect with size
	args := append([]string{"inspect", "--size", "--format", "{{json .}}"}, containerNames...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[docker:%s] batch inspect failed, trying one-by-one: %v", c.cfg.Name, err)
		return c.inspectOneByOne(containerNames)
	}

	var containers []DockerContainer
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ct DockerContainer
		if err := json.Unmarshal([]byte(line), &ct); err == nil {
			containers = append(containers, ct)
		}
	}
	return containers, nil
}

func (c *DockerCollector) inspectOneByOne(names []string) ([]DockerContainer, error) {
	var containers []DockerContainer
	for _, name := range names {
		cmd := exec.Command("docker", "inspect", "--size", "--format", "{{json .}}", name)
		out, err := cmd.Output()
		if err != nil {
			log.Printf("[docker:%s] inspect %s: %v", c.cfg.Name, name, err)
			continue
		}
		var ct DockerContainer
		if err := json.Unmarshal(out, &ct); err == nil {
			containers = append(containers, ct)
		}
	}
	return containers, nil
}

type dockerStats struct {
	cpuPercent float64
	memUsage   float64
	memLimit   float64
}

func (c *DockerCollector) getContainerStats(id string) (*dockerStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "stats", "--no-stream",
		"--format", `{{json .}}`, id)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	// JSON parsing (Docker 20.10+)
	var raw struct {
		CPUPerc  string `json:"CPUPerc"`
		MemUsage string `json:"MemUsage"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err == nil && raw.CPUPerc != "" {
		stats := &dockerStats{}
		cpuStr := strings.TrimSuffix(raw.CPUPerc, "%")
		if v, err := strconv.ParseFloat(cpuStr, 64); err == nil {
			stats.cpuPercent = v
		}
		memParts := strings.Split(raw.MemUsage, "/")
		if len(memParts) >= 1 {
			stats.memUsage = parseBytes(strings.TrimSpace(memParts[0]))
		}
		if len(memParts) >= 2 {
			stats.memLimit = parseBytes(strings.TrimSpace(memParts[1]))
		}
		return stats, nil
	}

	return nil, fmt.Errorf("unable to parse stats JSON from docker")
}

// parseBytes parses Docker-style human-readable byte strings like "113.2MiB", "19.63GiB", "1.5GB", "256B".
// Order matters: longer suffixes must be checked first, otherwise "MiB" matches "B" and fails.
func parseBytes(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Ordered from longest suffix to shortest so "MiB" matches before "B"
	type unit struct {
		suffix string
		mult   float64
	}
	units := []unit{
		{"TiB", 1 << 40},
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
		{"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000},
		{"KB", 1000},
		{"B", 1},
	}

	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			numStr := strings.TrimSuffix(s, u.suffix)
			num, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
			if err != nil {
				return 0
			}
			return num * u.mult
		}
	}

	// No unit -> assume bytes
	num, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return num
}

// ==================== Custom (Script) Collector ====================

type CustomCollector struct {
	cfg          TargetConfig
	defaults     DefaultConfig
	reg          *CustomMetricRegistry // global registry
	globalValues map[string]string
}

func NewCustomCollector(cfg TargetConfig, defaults DefaultConfig, reg *CustomMetricRegistry, globalValues map[string]string) *CustomCollector {
	return &CustomCollector{
		cfg: cfg, defaults: defaults, reg: reg, globalValues: globalValues,
	}
}

func (c *CustomCollector) Collect() {
	lv := prometheus.Labels{
		"name": c.cfg.Name,
		"env":  c.cfg.Labels["env"],
		"tier": c.cfg.Labels["tier"],
	}
	for k, v := range c.globalValues {
		lv[k] = v
	}

	// Use context with timeout to prevent hung scripts
	timeout := parseDuration(c.defaults.Timeout, "30s")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", c.cfg.Script)
	cmd.Env = append(os.Environ(), "TARGET_NAME="+c.cfg.Name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[custom:%s] script TIMEOUT (%v exceeded)", c.cfg.Name, timeout)
		} else {
			log.Printf("[custom:%s] script error: %v\n%s", c.cfg.Name, err, string(out))
		}
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			log.Printf("[custom:%s] parse error on %q: %v", c.cfg.Name, line, err)
			continue
		}
		gauge := c.reg.GetOrCreateGauge(fields[0])
		gauge.With(lv).Set(value)
	}
}

// ==================== Remote Collector ====================

type RemoteCollector struct {
	cfg      TargetConfig
	defaults DefaultConfig
	timeout  time.Duration
}

func NewRemoteCollector(cfg TargetConfig, defaults DefaultConfig) *RemoteCollector {
	return &RemoteCollector{cfg: cfg, defaults: defaults, timeout: parseDuration(defaults.Timeout, "30s")}
}

// Collect fetches remote /metrics text and parses it into MetricFamilies.
func (c *RemoteCollector) Collect() []*dto.MetricFamily {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.Remote.URL, nil)
	if err != nil {
		log.Printf("[remote:%s] create request: %v", c.cfg.Name, err)
		return nil
	}
	for k, v := range c.cfg.Remote.Headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[remote:%s] fetch failed: %v", c.cfg.Name, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[remote:%s] non-200 status: %d", c.cfg.Name, resp.StatusCode)
		return nil
	}

	parser := expfmt.NewDecoder(resp.Body, expfmt.NewFormat(expfmt.TypeTextPlain))
	var mfs []*dto.MetricFamily
	for {
		var mf dto.MetricFamily
		if err := parser.Decode(&mf); err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("[remote:%s] parse error: %v", c.cfg.Name, err)
			break
		}
		mfs = append(mfs, &mf)
	}
	log.Printf("[remote:%s] fetched %d metric families", c.cfg.Name, len(mfs))
	return mfs
}

// ==================== Snapshot Gatherer ====================

type snapshotGatherer struct {
	mu       sync.RWMutex
	snapshot []*dto.MetricFamily
	// Track remote metric names from previous round to prevent stale data
	remoteNames map[string]bool
}

func (g *snapshotGatherer) Gather() ([]*dto.MetricFamily, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.snapshot == nil {
		return nil, nil
	}
	return g.snapshot, nil
}

func (g *snapshotGatherer) Swap(mf []*dto.MetricFamily) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.snapshot = mf
}

// RemoveRemoteByName removes MetricFamilies whose names are in the given set.
func (g *snapshotGatherer) RemoveRemoteByName(names map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.snapshot) == 0 || len(names) == 0 {
		return
	}
	filtered := make([]*dto.MetricFamily, 0, len(g.snapshot))
	for _, mf := range g.snapshot {
		if mf.Name != nil && names[*mf.Name] {
			continue
		}
		filtered = append(filtered, mf)
	}
	g.snapshot = filtered
}

// ==================== Background Scraper ====================

type BackgroundScraper struct {
	mu               sync.RWMutex // protects collector slices
	urlCollectors    []*URLCollector
	dockerCollectors []*DockerCollector
	customCollectors []*CustomCollector
	remoteCollectors []*RemoteCollector
	remotePrevNames  map[string]bool
	urlMetrics       *URLMetrics
	dockerMetrics    *DockerMetrics
	customReg        *CustomMetricRegistry
	metricsReg       *prometheus.Registry
	mainReg          *prometheus.Registry
	snap             *snapshotGatherer
	stopCh           chan struct{}
	maxConcurrent    int
	scrapeInterval   time.Duration
}

func NewBackgroundScraper(
	urlCollectors []*URLCollector,
	dockerCollectors []*DockerCollector,
	customCollectors []*CustomCollector,
	remoteCollectors []*RemoteCollector,
	urlMetrics *URLMetrics,
	dockerMetrics *DockerMetrics,
	customReg *CustomMetricRegistry,
	metricsReg *prometheus.Registry,
	mainReg *prometheus.Registry,
	snap *snapshotGatherer,
	maxConcurrent int,
	scrapeInterval time.Duration,
) *BackgroundScraper {
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	return &BackgroundScraper{
		urlCollectors:    urlCollectors,
		dockerCollectors: dockerCollectors,
		customCollectors: customCollectors,
		remoteCollectors: remoteCollectors,
		remotePrevNames:  make(map[string]bool),
		urlMetrics:       urlMetrics,
		dockerMetrics:    dockerMetrics,
		customReg:        customReg,
		metricsReg:       metricsReg,
		mainReg:          mainReg,
		snap:             snap,
		stopCh:           make(chan struct{}),
		maxConcurrent:    maxConcurrent,
		scrapeInterval:   scrapeInterval,
	}
}

// Update atomically replaces the collector lists.
func (s *BackgroundScraper) Update(
	urlCollectors []*URLCollector,
	dockerCollectors []*DockerCollector,
	customCollectors []*CustomCollector,
	remoteCollectors []*RemoteCollector,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urlCollectors = urlCollectors
	s.dockerCollectors = dockerCollectors
	s.customCollectors = customCollectors
	s.remoteCollectors = remoteCollectors
	log.Printf("[scraper] collectors updated: url=%d, docker=%d, custom=%d, remote=%d",
		len(urlCollectors), len(dockerCollectors), len(customCollectors), len(remoteCollectors))
}

func (s *BackgroundScraper) scrapeOnce() {
	// Reset all metrics before collection
	s.urlMetrics.up.Reset()
	s.urlMetrics.statusCode.Reset()
	s.urlMetrics.duration.Reset()
	s.urlMetrics.bodyMatch.Reset()
	s.urlMetrics.statusMatch.Reset()
	s.urlMetrics.responseSize.Reset()
	s.dockerMetrics.up.Reset()
	s.dockerMetrics.cpuPercent.Reset()
	s.dockerMetrics.memUsage.Reset()
	s.dockerMetrics.memLimit.Reset()
	s.dockerMetrics.diskRw.Reset()
	s.dockerMetrics.diskRootFs.Reset()
	s.customReg.ResetAll() // reset all custom metrics

	// Snapshot current collector lists
	s.mu.RLock()
	urls := s.urlCollectors
	dockers := s.dockerCollectors
	customs := s.customCollectors
	remotes := s.remoteCollectors
	s.mu.RUnlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, s.maxConcurrent)

	for _, uc := range urls {
		wg.Add(1)
		go func(c *URLCollector) {
			defer wg.Done()
			sem <- struct{}{}
			c.Collect()
			<-sem
		}(uc)
	}

	for _, dc := range dockers {
		wg.Add(1)
		go func(c *DockerCollector) {
			defer wg.Done()
			sem <- struct{}{}
			c.Collect()
			<-sem
		}(dc)
	}

	for _, cc := range customs {
		wg.Add(1)
		go func(c *CustomCollector) {
			defer wg.Done()
			sem <- struct{}{}
			c.Collect()
			<-sem
		}(cc)
	}

	// Remote collectors: fetch raw metrics text and parse into MetricFamilies
	remoteMF := make([][]*dto.MetricFamily, len(remotes))
	for i, rc := range remotes {
		wg.Add(1)
		go func(idx int, c *RemoteCollector) {
			defer wg.Done()
			sem <- struct{}{}
			remoteMF[idx] = c.Collect()
			<-sem
		}(i, rc)
	}

	wg.Wait()

	// Purge custom gauges that were NOT seen in this round
	s.customReg.PurgeStale()

	// Atomically swap snapshot
	mf1, _ := s.metricsReg.Gather()
	all := mf1
	if s.mainReg != nil {
		mf2, _ := s.mainReg.Gather()
		all = append(mf1, mf2...)
	}
	// Remove stale remote metrics from previous round
	s.snap.RemoveRemoteByName(s.remotePrevNames)

	// Append fresh remote metric families
	for _, rMFs := range remoteMF {
		if rMFs != nil {
			all = append(all, rMFs...)
		}
	}
	s.snap.Swap(all)
	// Track remote metric names for next round stale cleanup
	s.remotePrevNames = make(map[string]bool)
	for _, rMFs := range remoteMF {
		for _, mf := range rMFs {
			if mf.Name != nil {
				s.remotePrevNames[*mf.Name] = true
			}
		}
	}
}

func (s *BackgroundScraper) Run() {
	log.Printf("[scraper] starting background scrape every %v", s.scrapeInterval)

	// Immediate first scrape
	s.scrapeOnce()

	ticker := time.NewTicker(s.scrapeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.scrapeOnce()
		case <-s.stopCh:
			log.Println("[scraper] stopped")
			return
		}
	}
}

func (s *BackgroundScraper) Stop() {
	close(s.stopCh)
}

// ==================== Main ====================

func buildCollectors(
	cfg *Config,
	urlMetrics *URLMetrics,
	dockerMetrics *DockerMetrics,
	customReg *CustomMetricRegistry,
	globalKeys []string,
	globalValues map[string]string,
) ([]*URLCollector, []*DockerCollector, []*CustomCollector, []*RemoteCollector) {
	var urlCollectors []*URLCollector
	var dockerCollectors []*DockerCollector
	var customCollectors []*CustomCollector
	var remoteCollectors []*RemoteCollector

	for _, t := range cfg.Targets {
		switch t.Type {
		case "url":
			log.Printf("Registering URL collector: %s -> %s", t.Name, t.URL)
			urlCollectors = append(urlCollectors, NewURLCollector(t, cfg.Defaults, urlMetrics, globalValues))
		case "docker":
			log.Printf("Registering Docker collector: %s (mode=%s)", t.Name, t.Mode)
			dockerCollectors = append(dockerCollectors, NewDockerCollector(t, dockerMetrics, globalKeys, globalValues))
		case "custom":
			log.Printf("Registering Custom collector: %s (script=%s)", t.Name, t.Script)
			customCollectors = append(customCollectors, NewCustomCollector(t, cfg.Defaults, customReg, globalValues))
		case "remote":
			log.Printf("Registering Remote collector: %s -> %s", t.Name, t.Remote.URL)
			remoteCollectors = append(remoteCollectors, NewRemoteCollector(t, cfg.Defaults))
		default:
			log.Printf("WARNING: unknown target type %q, skipping %s", t.Type, t.Name)
		}
	}
	return urlCollectors, dockerCollectors, customCollectors, remoteCollectors
}

func main() {
	configPath := "config.yaml"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = "config.yml"
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		exe, _ := os.Executable()
		dir := filepath.Dir(exe)
		configPath = filepath.Join(dir, "config.yaml")
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Loaded config: %d targets", len(cfg.Targets))

	// Separate registries: one for our metrics, one for go/process (optional)
	metricsReg := prometheus.NewRegistry()
	mainReg := prometheus.NewRegistry()
	exposeRuntime := cfg.Server.ExposeRuntimeMetrics == nil || *cfg.Server.ExposeRuntimeMetrics // default true
	if exposeRuntime {
		mainReg.MustRegister(prometheus.NewGoCollector())
		mainReg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
		log.Println("[config] runtime metrics (go/process) enabled")
	} else {
		log.Println("[config] runtime metrics (go/process) disabled")
	}
	// Pass nil for mainReg when runtime metrics disabled
	passMainReg := mainReg
	if !exposeRuntime {
		passMainReg = nil
	}

	globalKeys, globalValues := extractGlobalLabels(cfg.Defaults.GlobalLabels)
	log.Printf("[config] global labels: %v", globalValues)

	urlMetrics := NewURLMetrics(globalKeys)
	dockerMetrics := NewDockerMetrics(globalKeys)
	customReg := NewCustomMetricRegistry(metricsReg, globalKeys, globalValues)

	metricsReg.MustRegister(urlMetrics.up, urlMetrics.statusCode, urlMetrics.duration,
		urlMetrics.bodyMatch, urlMetrics.statusMatch, urlMetrics.responseSize)
	metricsReg.MustRegister(dockerMetrics.up, dockerMetrics.cpuPercent,
		dockerMetrics.memUsage, dockerMetrics.memLimit,
		dockerMetrics.diskRw, dockerMetrics.diskRootFs)
	// Custom metrics are registered dynamically via customReg

	// Initial build of collectors
	urlCollectors, dockerCollectors, customCollectors, remoteCollectors := buildCollectors(cfg, urlMetrics, dockerMetrics, customReg, globalKeys, globalValues)

	// Snapshot gatherer
	snap := &snapshotGatherer{}

	scrapeInterval := parseDuration(cfg.Defaults.ScrapeInterval, "5s")
	scraper := NewBackgroundScraper(
		urlCollectors, dockerCollectors, customCollectors, remoteCollectors,
		urlMetrics, dockerMetrics, customReg,
		metricsReg, passMainReg, snap,
		cfg.Server.MaxConcurrent,
		scrapeInterval,
	)
	go scraper.Run()

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("Server starting on %s (metrics: %s, pid: %d)", addr, cfg.Server.MetricsPath, os.Getpid())
	log.Printf("Send SIGHUP to reload config, SIGTERM to graceful shutdown")

	http.Handle(cfg.Server.MetricsPath, promhttp.HandlerFor(snap, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "OK - %d targets registered\n", len(cfg.Targets))
	})
	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(cfg)
	})

	srv := &http.Server{Addr: addr}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				log.Println("[config] SIGHUP received, reloading...")
				newCfg, err := loadConfig(configPath)
				if err != nil {
					log.Printf("[config] reload failed: %v", err)
					continue
				}
				// Rebuild collectors with new config
				newGlobalKeys, newGlobalValues := extractGlobalLabels(newCfg.Defaults.GlobalLabels)
				newURL, newDocker, newCustom, newRemote := buildCollectors(newCfg, urlMetrics, dockerMetrics, customReg, newGlobalKeys, newGlobalValues)
				// Atomically update the scraper
				scraper.Update(newURL, newDocker, newCustom, newRemote)
				cfg = newCfg // update global config for /config endpoint
				log.Printf("[config] reloaded: %d targets", len(cfg.Targets))

			case syscall.SIGTERM, syscall.SIGINT:
				log.Println("[server] shutting down...")
				scraper.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
				os.Exit(0)
			}
		}
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}