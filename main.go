package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

// ==================== Config ====================

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Defaults DefaultConfig  `yaml:"defaults"`
	Targets  []TargetConfig `yaml:"targets"`
}

type ServerConfig struct {
	Port        int    `yaml:"port"`
	MetricsPath string `yaml:"metrics_path"`
}

type DefaultConfig struct {
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
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
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	// 环境变量优先（方便 Docker 部署）
	if portEnv := os.Getenv("EXPORTER_PORT"); portEnv != "" {
		if p, err := strconv.Atoi(portEnv); err == nil && p > 0 {
			cfg.Server.Port = p
		}
	}
	if cfg.Server.MetricsPath == "" {
		cfg.Server.MetricsPath = "/metrics"
	}
	if cfg.Defaults.Interval == "" {
		cfg.Defaults.Interval = "30s"
	}
	if cfg.Defaults.Timeout == "" {
		cfg.Defaults.Timeout = "10s"
	}
	return &cfg, nil
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

// ==================== Shared Metrics ====================

type URLMetrics struct {
	up           *prometheus.GaugeVec
	statusCode   *prometheus.GaugeVec
	duration     *prometheus.GaugeVec
	bodyMatch    *prometheus.GaugeVec
	statusMatch  *prometheus.GaugeVec
	responseSize *prometheus.GaugeVec
}

func NewURLMetrics() *URLMetrics {
	extraLabels := []string{"env", "tier"}
	return &URLMetrics{
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_up", Help: "URL probe success (1=yes, 0=no)",
		}, append([]string{"name"}, extraLabels...)),
		statusCode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_http_status", Help: "HTTP response status code",
		}, append([]string{"name"}, extraLabels...)),
		duration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_duration_seconds", Help: "HTTP probe duration",
		}, append([]string{"name"}, extraLabels...)),
		bodyMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_body_match", Help: "Expected body content found (1=yes, 0=no)",
		}, append([]string{"name"}, extraLabels...)),
		statusMatch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_status_match", Help: "Expected status code matched (1=yes, 0=no)",
		}, append([]string{"name"}, extraLabels...)),
		responseSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "url_response_size_bytes", Help: "Response body size",
		}, append([]string{"name"}, extraLabels...)),
	}
}

type DockerMetrics struct {
	up         *prometheus.GaugeVec
	cpuPercent *prometheus.GaugeVec
	memUsage   *prometheus.GaugeVec
	memLimit   *prometheus.GaugeVec
}

func NewDockerMetrics() *DockerMetrics {
	extraLabels := []string{"env", "tier"}
	return &DockerMetrics{
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_up", Help: "Container is running (1=yes, 0=no)",
		}, append([]string{"container", "image"}, extraLabels...)),
		cpuPercent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_cpu_percent", Help: "Container CPU usage percent",
		}, append([]string{"container", "image"}, extraLabels...)),
		memUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_memory_usage_bytes", Help: "Container memory usage",
		}, append([]string{"container", "image"}, extraLabels...)),
		memLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docker_container_memory_limit_bytes", Help: "Container memory limit",
		}, append([]string{"container", "image"}, extraLabels...)),
	}
}

// ==================== URL Collector ====================

type URLCollector struct {
	cfg      TargetConfig
	defaults DefaultConfig
	metrics  *URLMetrics
}

func NewURLCollector(cfg TargetConfig, defaults DefaultConfig, m *URLMetrics) *URLCollector {
	return &URLCollector{cfg: cfg, defaults: defaults, metrics: m}
}

func (c *URLCollector) Collect() {
	// Reset stale metrics before each scrape
	c.metrics.up.Reset()
	c.metrics.statusCode.Reset()
	c.metrics.duration.Reset()
	c.metrics.bodyMatch.Reset()
	c.metrics.statusMatch.Reset()
	c.metrics.responseSize.Reset()

	timeout := parseDuration(c.defaults.Timeout, "10s")
	lv := prometheus.Labels{
		"name": c.cfg.Name,
		"env":  c.cfg.Labels["env"],
		"tier": c.cfg.Labels["tier"],
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

	req, err := http.NewRequest(method, c.cfg.URL, nil)
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

	body, _ := io.ReadAll(resp.Body)
	c.metrics.up.With(lv).Set(1)
	c.metrics.statusCode.With(lv).Set(float64(resp.StatusCode))
	c.metrics.duration.With(lv).Set(duration)
	c.metrics.responseSize.With(lv).Set(float64(len(body)))

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
	ID     string        `json:"Id"`
	Name   string        `json:"Name"`
	State  DockerState   `json:"State"`
	Image  string        `json:"Image"`
}

type DockerState struct {
	Status string `json:"Status"`
}

type DockerCollector struct {
	cfg     TargetConfig
	metrics *DockerMetrics
}

// ==================== Custom (Script) Collector ====================

type CustomCollector struct {
	cfg      TargetConfig
	defaults DefaultConfig
	gauges   map[string]*prometheus.GaugeVec
	mu       sync.Mutex
	reg      *prometheus.Registry
}

func NewCustomCollector(cfg TargetConfig, defaults DefaultConfig, reg *prometheus.Registry) *CustomCollector {
	return &CustomCollector{
		cfg: cfg, defaults: defaults, reg: reg,
		gauges: make(map[string]*prometheus.GaugeVec),
	}
}

func (c *CustomCollector) getOrCreateGauge(name string) *prometheus.GaugeVec {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g, ok := c.gauges[name]; ok {
		return g
	}
	labels := []string{"name", "env", "tier"}
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: name,
		Help: "Custom metric from script: " + c.cfg.Name,
	}, labels)
	c.gauges[name] = g
	c.reg.MustRegister(g)
	return g
}

func (c *CustomCollector) Collect() {
	c.mu.Lock()
	for _, g := range c.gauges {
		g.Reset()
	}
	c.mu.Unlock()

	lv := prometheus.Labels{
		"name": c.cfg.Name,
		"env":  c.cfg.Labels["env"],
		"tier": c.cfg.Labels["tier"],
	}

	cmd := exec.Command("sh", "-c", c.cfg.Script)
	cmd.Env = append(os.Environ(), "TARGET_NAME="+c.cfg.Name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[custom:%s] script error: %v\n%s", c.cfg.Name, err, string(out))
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
		c.getOrCreateGauge(fields[0]).With(lv).Set(value)
	}
}

func NewDockerCollector(cfg TargetConfig, m *DockerMetrics) *DockerCollector {
	return &DockerCollector{cfg: cfg, metrics: m}
}

func (c *DockerCollector) Collect() {
	containers, err := c.listContainers()
	if err != nil {
		log.Printf("[docker] list containers: %v", err)
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
		c.metrics.up.WithLabelValues(lv...).Set(float64(isRunning))

		if state == "running" {
			stats, err := c.getContainerStats(ct.ID)
			if err == nil && stats != nil {
				if stats.cpuPercent > 0 {
					c.metrics.cpuPercent.WithLabelValues(lv...).Set(stats.cpuPercent)
				}
				if stats.memUsage > 0 {
					c.metrics.memUsage.WithLabelValues(lv...).Set(stats.memUsage)
				}
				if stats.memLimit > 0 {
					c.metrics.memLimit.WithLabelValues(lv...).Set(stats.memLimit)
				}
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

	var containers []DockerContainer
	for _, name := range containerNames {
		cmd := exec.Command("docker", "inspect", "--format", "{{json .}}", name)
		out, err := cmd.Output()
		if err != nil {
			log.Printf("[docker] inspect %s: %v", name, err)
			continue
		}
		var ct DockerContainer
		if err := json.Unmarshal(bytes.TrimSpace(out), &ct); err == nil {
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
	cmd := exec.Command("docker", "stats", "--no-stream", "--format",
		"{{.CPUPerc}}\t{{.MemUsage}}", id)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(parts) < 2 {
		return nil, fmt.Errorf("unexpected stats format")
	}
	stats := &dockerStats{}
	cpuStr := strings.TrimSuffix(parts[0], "%")
	if v, err := strconv.ParseFloat(cpuStr, 64); err == nil {
		stats.cpuPercent = v
	}
	memParts := strings.Split(parts[1], "/")
	if len(memParts) >= 1 {
		stats.memUsage = parseBytes(strings.TrimSpace(memParts[0]))
	}
	if len(memParts) >= 2 {
		stats.memLimit = parseBytes(strings.TrimSpace(memParts[1]))
	}
	return stats, nil
}

func parseBytes(s string) float64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	var multiplier float64 = 1
	switch {
	case strings.HasSuffix(s, "GIB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GIB")
	case strings.HasSuffix(s, "MIB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MIB")
	case strings.HasSuffix(s, "KIB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KIB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v * multiplier
}

// ==================== Main ====================

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

	// Separate registries for ordering control
	metricsReg := prometheus.NewRegistry()    // GaugeVec metrics
	mainReg := prometheus.NewRegistry()        // Go + process collectors
	mainReg.MustRegister(prometheus.NewGoCollector())
	mainReg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	urlMetrics := NewURLMetrics()
	dockerMetrics := NewDockerMetrics()

	// Register GaugeVecs to metricsReg
	metricsReg.MustRegister(urlMetrics.up, urlMetrics.statusCode, urlMetrics.duration,
		urlMetrics.bodyMatch, urlMetrics.statusMatch, urlMetrics.responseSize)
	metricsReg.MustRegister(dockerMetrics.up, dockerMetrics.cpuPercent,
		dockerMetrics.memUsage, dockerMetrics.memLimit)

	var urlCollectors []*URLCollector
	var dockerCollectors []*DockerCollector
	var customCollectors []*CustomCollector

	for _, t := range cfg.Targets {
		switch t.Type {
		case "url":
			log.Printf("Registering URL collector: %s -> %s", t.Name, t.URL)
			urlCollectors = append(urlCollectors, NewURLCollector(t, cfg.Defaults, urlMetrics))
		case "docker":
			log.Printf("Registering Docker collector: %s (mode=%s)", t.Name, t.Mode)
			dockerCollectors = append(dockerCollectors, NewDockerCollector(t, dockerMetrics))
		case "custom":
			log.Printf("Registering Custom collector: %s (script=%s)", t.Name, t.Script)
			customCollectors = append(customCollectors, NewCustomCollector(t, cfg.Defaults, metricsReg))
		default:
			log.Printf("WARNING: unknown target type %q, skipping %s", t.Type, t.Name)
		}
	}

	gatherer := &orderedGatherer{
		metricsReg:       metricsReg,
		mainReg:          mainReg,
		urlMetrics:       urlMetrics,
		dockerMetrics:    dockerMetrics,
		urlCollectors:    urlCollectors,
		dockerCollectors: dockerCollectors,
		customCollectors: customCollectors,
	}

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("Server starting on %s (metrics: %s)", addr, cfg.Server.MetricsPath)

	http.Handle(cfg.Server.MetricsPath, promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "OK - %d targets registered\n", len(cfg.Targets))
	})
	log.Fatal(http.ListenAndServe(addr, nil))
}

// orderedGatherer runs probes first, then gathers all metrics
type orderedGatherer struct {
	metricsReg       *prometheus.Registry
	mainReg          *prometheus.Registry
	urlMetrics       *URLMetrics
	dockerMetrics    *DockerMetrics
	urlCollectors    []*URLCollector
	dockerCollectors []*DockerCollector
	customCollectors []*CustomCollector
}

func (g *orderedGatherer) Gather() ([]*dto.MetricFamily, error) {
	// Step 0: Reset all shared GaugeVecs before probing
	g.dockerMetrics.up.Reset()
	g.dockerMetrics.cpuPercent.Reset()
	g.dockerMetrics.memUsage.Reset()
	g.dockerMetrics.memLimit.Reset()
	g.urlMetrics.up.Reset()
	g.urlMetrics.statusCode.Reset()
	g.urlMetrics.duration.Reset()
	g.urlMetrics.bodyMatch.Reset()
	g.urlMetrics.statusMatch.Reset()
	g.urlMetrics.responseSize.Reset()

	// Step 1: Run all probes (sets GaugeVec values)
	for _, uc := range g.urlCollectors {
		uc.Collect()
	}
	for _, dc := range g.dockerCollectors {
		dc.Collect()
	}
	for _, cc := range g.customCollectors {
		cc.Collect()
	}

	// Step 2: Gather metrics
	mf1, err := g.metricsReg.Gather()
	if err != nil {
		return mf1, err
	}
	mf2, err := g.mainReg.Gather()
	if err != nil {
		return append(mf1, mf2...), nil
	}
	return append(mf1, mf2...), nil
}
