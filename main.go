package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	instancesURL = "https://github.com/EduardPrigoana/monochrome/raw/refs/heads/main/instances.json"
	checkTimeout = 10 * time.Second
	maxLatency   = checkTimeout
)

var (
	logger     *slog.Logger
	bufferPool = sync.Pool{
		New: func() interface{} {
			buf := make([]byte, 32*1024)
			return &buf
		},
	}
)

type ProxyError struct {
	Code    int
	Message string
	Err     error
}

func (e *ProxyError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

type InstancePerformance struct {
	URL     string
	Latency time.Duration
}

type InstanceHealth struct {
	failures    int
	lastFailure time.Time
	lastSuccess time.Time
	totalReqs   int64
	successReqs int64
	mu          sync.RWMutex
}

type Config struct {
	Port                string
	CacheTTL            time.Duration
	HealthCheckInterval time.Duration
	RequestTimeout      time.Duration
	MaxRetries          int
}

func loadConfig() (Config, error) {
	logger.Info("loading configuration from environment")
	cfg := Config{
		Port:                getEnv("PORT", "8080"),
		CacheTTL:            getDurationEnv("CACHE_TTL", 2*time.Hour),
		HealthCheckInterval: getDurationEnv("HEALTH_CHECK_INTERVAL", 30*time.Minute),
		RequestTimeout:      getDurationEnv("REQUEST_TIMEOUT", 30*time.Second),
		MaxRetries:          getIntEnv("MAX_RETRIES", 3),
	}

	logger.Info("configuration loaded",
		"port", cfg.Port,
		"cache_ttl", cfg.CacheTTL,
		"health_check_interval", cfg.HealthCheckInterval,
		"request_timeout", cfg.RequestTimeout,
		"max_retries", cfg.MaxRetries)

	if err := cfg.Validate(); err != nil {
		logger.Error("configuration validation failed", "error", err)
		return cfg, fmt.Errorf("invalid config: %w", err)
	}

	logger.Info("configuration validated successfully")
	return cfg, nil
}

func (c *Config) Validate() error {
	logger.Debug("validating configuration")
	if c.CacheTTL < 0 {
		return fmt.Errorf("cache TTL must be positive, got: %v", c.CacheTTL)
	}
	if c.HealthCheckInterval < time.Minute {
		return fmt.Errorf("health check interval must be at least 1 minute, got: %v", c.HealthCheckInterval)
	}
	if c.RequestTimeout < time.Second {
		return fmt.Errorf("request timeout must be at least 1 second, got: %v", c.RequestTimeout)
	}
	if c.MaxRetries < 1 {
		return fmt.Errorf("max retries must be at least 1, got: %d", c.MaxRetries)
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value != "" {
		logger.Debug("environment variable found", "key", key, "value", value)
		return value
	}
	logger.Debug("environment variable not found, using default", "key", key, "default", defaultValue)
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			logger.Debug("duration environment variable parsed", "key", key, "value", d)
			return d
		}
		logger.Warn("failed to parse duration environment variable", "key", key, "value", value, "using_default", defaultValue)
	}
	logger.Debug("duration environment variable not found, using default", "key", key, "default", defaultValue)
	return defaultValue
}

func getIntEnv(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			logger.Debug("int environment variable parsed", "key", key, "value", i)
			return i
		}
		logger.Warn("failed to parse int environment variable", "key", key, "value", value, "using_default", defaultValue)
	}
	logger.Debug("int environment variable not found, using default", "key", key, "default", defaultValue)
	return defaultValue
}

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

type LocalCache struct {
	data      map[string]cacheEntry
	mu        sync.RWMutex
	hits      int64
	misses    int64
	evictions int64
}

type cacheEntry struct {
	value      []byte
	expiration time.Time
	accessTime time.Time
	size       int
}

func NewLocalCache() *LocalCache {
	logger.Info("initializing local cache")
	lc := &LocalCache{
		data: make(map[string]cacheEntry),
	}
	go lc.cleanup()
	logger.Info("local cache initialized successfully")
	return lc
}

func (l *LocalCache) Get(ctx context.Context, key string) ([]byte, error) {
	logger.Debug("cache get operation started", "key", key)
	
	select {
	case <-ctx.Done():
		logger.Warn("cache get operation cancelled", "key", key, "error", ctx.Err())
		return nil, ctx.Err()
	default:
	}

	l.mu.RLock()
	entry, exists := l.data[key]
	l.mu.RUnlock()

	if !exists {
		l.mu.Lock()
		l.misses++
		misses := l.misses
		l.mu.Unlock()
		logger.Debug("cache miss", "key", key, "total_misses", misses)
		return nil, nil
	}

	if !entry.expiration.IsZero() && time.Now().After(entry.expiration) {
		l.mu.Lock()
		delete(l.data, key)
		l.misses++
		l.evictions++
		misses := l.misses
		evictions := l.evictions
		l.mu.Unlock()
		logger.Debug("cache entry expired", "key", key, "expiration", entry.expiration, "total_misses", misses, "total_evictions", evictions)
		return nil, nil
	}

	l.mu.Lock()
	l.hits++
	hits := l.hits
	entry.accessTime = time.Now()
	l.data[key] = entry
	l.mu.Unlock()

	logger.Debug("cache hit", "key", key, "size", entry.size, "total_hits", hits, "age", time.Since(entry.accessTime))
	return entry.value, nil
}

func (l *LocalCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	logger.Debug("cache set operation started", "key", key, "size", len(value), "ttl", ttl)
	
	select {
	case <-ctx.Done():
		logger.Warn("cache set operation cancelled", "key", key, "error", ctx.Err())
		return ctx.Err()
	default:
	}

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}

	l.mu.Lock()
	l.data[key] = cacheEntry{
		value:      value,
		expiration: exp,
		accessTime: time.Now(),
		size:       len(value),
	}
	totalEntries := len(l.data)
	l.mu.Unlock()

	logger.Debug("cache set operation completed", "key", key, "expiration", exp, "total_entries", totalEntries)
	return nil
}

func (l *LocalCache) cleanup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	
	logger.Info("cache cleanup routine started", "interval", time.Hour)

	for range ticker.C {
		logger.Info("starting cache cleanup cycle")
		startTime := time.Now()
		
		l.mu.Lock()
		now := time.Now()
		cleaned := 0
		totalSize := 0
		
		for key, entry := range l.data {
			totalSize += entry.size
			if !entry.expiration.IsZero() && now.After(entry.expiration) {
				delete(l.data, key)
				cleaned++
				l.evictions++
				logger.Debug("cleaned expired cache entry", "key", key, "size", entry.size, "expired_at", entry.expiration)
			}
		}
		
		remaining := len(l.data)
		evictions := l.evictions
		hits := l.hits
		misses := l.misses
		l.mu.Unlock()

		duration := time.Since(startTime)
		logger.Info("cache cleanup cycle completed",
			"duration", duration,
			"cleaned_entries", cleaned,
			"remaining_entries", remaining,
			"total_size_bytes", totalSize,
			"total_evictions", evictions,
			"total_hits", hits,
			"total_misses", misses)
	}
}

type ProxyServer struct {
	cache          Cache
	instances      []string
	instanceHealth map[string]*InstanceHealth
	mu             sync.RWMutex
	client         *http.Client
	config         Config
	totalRequests  int64
	reqMu          sync.Mutex
}

func loadInstances(url string) ([]string, error) {
	logger.Info("loading instances from remote source", "url", url)
	startTime := time.Now()
	
	resp, err := http.Get(url)
	if err != nil {
		logger.Error("failed to fetch instances", "url", url, "error", err)
		return nil, err
	}
	defer resp.Body.Close()

	logger.Debug("instances fetch response received", "url", url, "status", resp.StatusCode, "content_type", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read instances response body", "url", url, "error", err)
		return nil, err
	}

	logger.Debug("instances response body read", "url", url, "size", len(body))

	var instances []string
	if err := json.Unmarshal(body, &instances); err != nil {
		logger.Error("failed to unmarshal instances JSON", "url", url, "error", err, "body_size", len(body))
		return nil, err
	}

	duration := time.Since(startTime)
	logger.Info("instances loaded successfully", "url", url, "count", len(instances), "duration", duration)
	
	for i, instance := range instances {
		logger.Debug("loaded instance", "index", i, "url", instance)
	}

	return instances, nil
}

func checkAndSortInstances(instances []string) []string {
	logger.Info("starting instance performance check", "total_instances", len(instances))
	startTime := time.Now()

	var wg sync.WaitGroup
	performanceResults := make([]InstancePerformance, 0, len(instances))
	var mu sync.Mutex

	client := &http.Client{
		Timeout: checkTimeout,
	}

	for idx, instance := range instances {
		wg.Add(1)
		go func(instanceURL string, index int) {
			defer wg.Done()

			testURL := strings.TrimSuffix(instanceURL, "/") + "/search/?s=ilybasement"
			logger.Debug("checking instance performance", "instance", instanceURL, "index", index, "test_url", testURL)

			start := time.Now()
			resp, err := client.Get(testURL)
			latency := time.Since(start)

			currentPerf := InstancePerformance{URL: instanceURL}

			if err != nil {
				logger.Warn("instance health check failed", "instance", instanceURL, "index", index, "error", err, "latency", latency)
				currentPerf.Latency = maxLatency
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					logger.Info("instance health check succeeded", "instance", instanceURL, "index", index, "status", resp.StatusCode, "latency", latency)
					currentPerf.Latency = latency
				} else {
					logger.Warn("instance health check returned non-2xx", "instance", instanceURL, "index", index, "status", resp.StatusCode, "latency", latency)
					currentPerf.Latency = maxLatency
				}
			}

			mu.Lock()
			performanceResults = append(performanceResults, currentPerf)
			mu.Unlock()

		}(instance, idx)
	}

	wg.Wait()

	sort.Slice(performanceResults, func(i, j int) bool {
		return performanceResults[i].Latency < performanceResults[j].Latency
	})

	sortedInstanceURLs := make([]string, 0, len(instances))
	totalDuration := time.Since(startTime)
	
	logger.Info("instance performance check complete", "total_instances", len(instances), "total_duration", totalDuration)
	
	healthyCount := 0
	for i, perf := range performanceResults {
		sortedInstanceURLs = append(sortedInstanceURLs, perf.URL)
		if perf.Latency >= maxLatency {
			logger.Warn("instance ranked unhealthy", "rank", i+1, "url", perf.URL, "status", "FAILED or TIMEOUT", "latency", perf.Latency)
		} else {
			healthyCount++
			logger.Info("instance ranked healthy", "rank", i+1, "url", perf.URL, "latency", perf.Latency)
		}
	}

	logger.Info("instance sorting summary", "total", len(instances), "healthy", healthyCount, "unhealthy", len(instances)-healthyCount)

	return sortedInstanceURLs
}

func isAllowedPath(path string) bool {
	logger.Debug("checking if path is allowed", "path", path)
	
	if path == "/" || path == "/health" {
		logger.Debug("path allowed", "path", path, "reason", "root or health endpoint")
		return true
	}

	allowedPrefixes := []string{
		"/track/", "/dash/", "/search/", "/cover/", "/song/",
		"/album/", "/playlist/", "/artist/", "/lyrics/", "/home/", "/mix/",
	}

	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(path, prefix) {
			logger.Debug("path allowed", "path", path, "matched_prefix", prefix)
			return true
		}
	}

	logger.Warn("path not allowed", "path", path)
	return false
}

func (p *ProxyServer) getInstances() []string {
	p.mu.RLock()
	instances := append([]string{}, p.instances...)
	p.mu.RUnlock()
	logger.Debug("retrieved instances list", "count", len(instances))
	return instances
}

func (p *ProxyServer) updateInstances(instances []string) {
	p.mu.Lock()
	oldCount := len(p.instances)
	p.instances = instances
	newCount := len(p.instances)
	p.mu.Unlock()
	logger.Info("instances list updated", "old_count", oldCount, "new_count", newCount)
}

func (p *ProxyServer) startHealthChecker() {
	logger.Info("starting health checker routine", "interval", p.config.HealthCheckInterval)
	ticker := time.NewTicker(p.config.HealthCheckInterval)
	go func() {
		for tickTime := range ticker.C {
			logger.Info("health check triggered", "time", tickTime)
			instances := p.getInstances()
			sortedInstances := checkAndSortInstances(instances)
			p.updateInstances(sortedInstances)
			logger.Info("health check completed", "next_check", tickTime.Add(p.config.HealthCheckInterval))
		}
	}()
}

func (p *ProxyServer) isHealthy(instance string) bool {
	p.mu.RLock()
	health := p.instanceHealth[instance]
	p.mu.RUnlock()

	if health == nil {
		logger.Debug("instance health check: no health record", "instance", instance, "result", "healthy")
		return true
	}

	health.mu.RLock()
	failures := health.failures
	lastFailure := health.lastFailure
	lastSuccess := health.lastSuccess
	totalReqs := health.totalReqs
	successReqs := health.successReqs
	health.mu.RUnlock()

	timeSinceFailure := time.Since(lastFailure)
	isHealthy := failures < 3 || timeSinceFailure >= 5*time.Minute

	logger.Debug("instance health check",
		"instance", instance,
		"failures", failures,
		"last_failure", lastFailure,
		"last_success", lastSuccess,
		"time_since_failure", timeSinceFailure,
		"total_requests", totalReqs,
		"success_requests", successReqs,
		"result", isHealthy)

	return isHealthy
}

func (p *ProxyServer) recordFailure(instance string) {
	p.mu.Lock()
	if p.instanceHealth[instance] == nil {
		p.instanceHealth[instance] = &InstanceHealth{}
		logger.Debug("created new health record for instance", "instance", instance)
	}
	health := p.instanceHealth[instance]
	p.mu.Unlock()

	health.mu.Lock()
	health.failures++
	health.lastFailure = time.Now()
	health.totalReqs++
	failures := health.failures
	totalReqs := health.totalReqs
	successReqs := health.successReqs
	health.mu.Unlock()

	successRate := float64(0)
	if totalReqs > 0 {
		successRate = float64(successReqs) / float64(totalReqs) * 100
	}

	logger.Warn("instance failure recorded",
		"instance", instance,
		"failures", failures,
		"total_requests", totalReqs,
		"success_requests", successReqs,
		"success_rate", fmt.Sprintf("%.2f%%", successRate))
}

func (p *ProxyServer) recordSuccess(instance string) {
	p.mu.Lock()
	if p.instanceHealth[instance] == nil {
		p.instanceHealth[instance] = &InstanceHealth{}
		logger.Debug("created new health record for instance", "instance", instance)
	}
	health := p.instanceHealth[instance]
	p.mu.Unlock()

	health.mu.Lock()
	previousFailures := health.failures
	health.failures = 0
	health.lastSuccess = time.Now()
	health.totalReqs++
	health.successReqs++
	totalReqs := health.totalReqs
	successReqs := health.successReqs
	health.mu.Unlock()

	successRate := float64(0)
	if totalReqs > 0 {
		successRate = float64(successReqs) / float64(totalReqs) * 100
	}

	if previousFailures > 0 {
		logger.Info("instance recovered from failures",
			"instance", instance,
			"previous_failures", previousFailures,
			"total_requests", totalReqs,
			"success_requests", successReqs,
			"success_rate", fmt.Sprintf("%.2f%%", successRate))
	} else {
		logger.Debug("instance success recorded",
			"instance", instance,
			"total_requests", totalReqs,
			"success_requests", successReqs,
			"success_rate", fmt.Sprintf("%.2f%%", successRate))
	}
}

func (p *ProxyServer) fetchWithRetry(ctx context.Context, url string) (*http.Response, error) {
	logger.Debug("starting fetch with retry", "url", url, "max_retries", p.config.MaxRetries)
	
	var resp *http.Response
	var err error

	for i := 0; i < p.config.MaxRetries; i++ {
		attemptStart := time.Now()
		logger.Debug("fetch attempt started", "url", url, "attempt", i+1, "max_retries", p.config.MaxRetries)
		
		req, reqErr := http.NewRequestWithContext(ctx, "GET", url, nil)
		if reqErr != nil {
			logger.Error("failed to create request", "url", url, "attempt", i+1, "error", reqErr)
			return nil, reqErr
		}

		resp, err = p.client.Do(req)
		attemptDuration := time.Since(attemptStart)

		if err == nil && resp.StatusCode < 500 {
			logger.Info("fetch attempt succeeded",
				"url", url,
				"attempt", i+1,
				"status", resp.StatusCode,
				"duration", attemptDuration,
				"content_length", resp.ContentLength)
			return resp, nil
		}

		if resp != nil {
			logger.Warn("fetch attempt failed with response",
				"url", url,
				"attempt", i+1,
				"status", resp.StatusCode,
				"duration", attemptDuration,
				"error", err)
			resp.Body.Close()
		} else {
			logger.Warn("fetch attempt failed without response",
				"url", url,
				"attempt", i+1,
				"duration", attemptDuration,
				"error", err)
		}

		if i < p.config.MaxRetries-1 {
			backoff := time.Duration(1<<uint(i)) * time.Second
			logger.Info("retrying request with backoff",
				"url", url,
				"attempt", i+1,
				"backoff", backoff,
				"next_attempt", i+2)
			
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				logger.Warn("fetch retry cancelled by context", "url", url, "error", ctx.Err())
				return nil, ctx.Err()
			}
		}
	}

	logger.Error("all fetch attempts failed", "url", url, "total_attempts", p.config.MaxRetries, "last_error", err)
	return nil, err
}

func (p *ProxyServer) setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	logger.Debug("CORS headers set")
}

func (p *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()
	ctx := r.Context()
	
	p.reqMu.Lock()
	p.totalRequests++
	reqID := p.totalRequests
	p.reqMu.Unlock()

	logger.Info("request received",
		"request_id", reqID,
		"method", r.Method,
		"path", r.URL.Path,
		"query", r.URL.RawQuery,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
		"referer", r.Referer())

	p.setCORSHeaders(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		logger.Info("OPTIONS request handled", "request_id", reqID, "duration", time.Since(requestStart))
		return
	}

	if r.Method != "GET" {
		logger.Warn("method not allowed", "request_id", reqID, "method", r.Method, "path", r.URL.Path)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !isAllowedPath(r.URL.Path) {
		logger.Warn("path not allowed", "request_id", reqID, "path", r.URL.Path)
		http.Error(w, "Path not allowed", http.StatusForbidden)
		return
	}

	cacheKey := r.URL.Path
	if r.URL.RawQuery != "" {
		cacheKey += "?" + r.URL.RawQuery
	}

	logger.Debug("checking cache", "request_id", reqID, "cache_key", cacheKey)
	cached, err := p.cache.Get(ctx, cacheKey)
	if err != nil {
		logger.Error("cache get error", "request_id", reqID, "error", err, "key", cacheKey)
	}

	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Write(cached)
		duration := time.Since(requestStart)
		logger.Info("request served from cache",
			"request_id", reqID,
			"cache_key", cacheKey,
			"response_size", len(cached),
			"duration", duration)
		return
	}

	logger.Debug("cache miss, fetching from instances", "request_id", reqID, "cache_key", cacheKey)

	var lastErr error
	var lastResponse *http.Response
	var lastBody []byte
	instances := p.getInstances()

	logger.Debug("attempting to fetch from instances", "request_id", reqID, "total_instances", len(instances))

	for idx, instance := range instances {
		if !p.isHealthy(instance) {
			logger.Debug("skipping unhealthy instance",
				"request_id", reqID,
				"instance", instance,
				"index", idx,
				"total_instances", len(instances))
			continue
		}

		url := strings.TrimSuffix(instance, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}

		logger.Info("attempting instance",
			"request_id", reqID,
			"instance", instance,
			"index", idx,
			"total_instances", len(instances),
			"url", url)

		resp, err := p.fetchWithRetry(ctx, url)
		if err != nil {
			lastErr = err
			p.recordFailure(instance)
			logger.Warn("instance request failed",
				"request_id", reqID,
				"instance", instance,
				"index", idx,
				"error", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = err
			p.recordFailure(instance)
			logger.Error("failed to read response body",
				"request_id", reqID,
				"instance", instance,
				"index", idx,
				"error", err,
				"status", resp.StatusCode)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.recordSuccess(instance)

			logger.Debug("setting cache", "request_id", reqID, "cache_key", cacheKey, "size", len(body), "ttl", p.config.CacheTTL)
			if err := p.cache.Set(ctx, cacheKey, body, p.config.CacheTTL); err != nil {
				logger.Error("cache set error", "request_id", reqID, "error", err, "key", cacheKey)
			}

			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			
			duration := time.Since(requestStart)
			logger.Info("request completed successfully",
				"request_id", reqID,
				"instance", instance,
				"index", idx,
				"status", resp.StatusCode,
				"response_size", len(body),
				"content_type", contentType,
				"duration", duration)
			return
		}

		p.recordFailure(instance)
		lastResponse = resp
		lastBody = body
		logger.Warn("instance returned non-2xx status",
			"request_id", reqID,
			"instance", instance,
			"index", idx,
			"status", resp.StatusCode,
			"response_size", len(body))
	}

	if lastResponse != nil {
		contentType := lastResponse.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(lastResponse.StatusCode)
		w.Write(lastBody)
		
		duration := time.Since(requestStart)
		logger.Warn("request failed, returning last error response",
			"request_id", reqID,
			"status", lastResponse.StatusCode,
			"response_size", len(lastBody),
			"duration", duration)
		return
	}

	duration := time.Since(requestStart)
	if lastErr != nil {
		logger.Error("all instances failed with error",
			"request_id", reqID,
			"error", lastErr,
			"total_instances", len(instances),
			"duration", duration)
		http.Error(w, fmt.Sprintf("All instances failed: %v", lastErr), http.StatusBadGateway)
	} else {
		logger.Error("no instances available",
			"request_id", reqID,
			"total_instances", len(instances),
			"duration", duration)
		http.Error(w, "No instances available", http.StatusServiceUnavailable)
	}
}

func (p *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	logger.Info("health check endpoint called", "remote_addr", r.RemoteAddr)
	
	w.Header().Set("Content-Type", "application/json")

	instances := p.getInstances()
	healthyCount := 0
	healthDetails := make([]map[string]interface{}, 0, len(instances))

	for idx, instance := range instances {
		isHealthy := p.isHealthy(instance)
		if isHealthy {
			healthyCount++
		}
		
		p.mu.RLock()
		health := p.instanceHealth[instance]
		p.mu.RUnlock()
		
		instanceData := map[string]interface{}{
			"instance_id": idx + 1,
			"healthy":     isHealthy,
		}
		
		if health != nil {
			health.mu.RLock()
			instanceData["failures"] = health.failures
			instanceData["last_failure"] = health.lastFailure
			instanceData["last_success"] = health.lastSuccess
			instanceData["total_reqs"] = health.totalReqs
			instanceData["success_reqs"] = health.successReqs
			
			if health.totalReqs > 0 {
				successRate := float64(health.successReqs) / float64(health.totalReqs) * 100
				instanceData["success_rate"] = fmt.Sprintf("%.2f%%", successRate)
			} else {
				instanceData["success_rate"] = "0.00%"
			}
			health.mu.RUnlock()
			
			logger.Debug("health check instance details",
				"instance_id", idx+1,
				"instance_url", instance,
				"healthy", isHealthy,
				"failures", health.failures,
				"total_reqs", health.totalReqs,
				"success_reqs", health.successReqs)
		} else {
			instanceData["failures"] = 0
			instanceData["last_failure"] = nil
			instanceData["last_success"] = nil
			instanceData["total_reqs"] = 0
			instanceData["success_reqs"] = 0
			instanceData["success_rate"] = "0.00%"
			
			logger.Debug("health check instance details",
				"instance_id", idx+1,
				"instance_url", instance,
				"healthy", isHealthy,
				"note", "no health record")
		}
		
		healthDetails = append(healthDetails, instanceData)
	}

	p.reqMu.Lock()
	totalReqs := p.totalRequests
	p.reqMu.Unlock()

	status := map[string]interface{}{
		"status":              "ok",
		"total_instances":     len(instances),
		"healthy_instances":   healthyCount,
		"unhealthy_instances": len(instances) - healthyCount,
		"total_requests":      totalReqs,
		"instances":           healthDetails,
	}

	json.NewEncoder(w).Encode(status)
	
	logger.Info("health check response sent",
		"total_instances", len(instances),
		"healthy_instances", healthyCount,
		"unhealthy_instances", len(instances)-healthyCount,
		"total_requests", totalReqs)
}

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	logger.Info("application starting", "pid", os.Getpid())

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger.Info("fetching initial instances list")
	initialInstances, err := loadInstances(instancesURL)
	if err != nil {
		logger.Error("failed to load instances", "error", err, "url", instancesURL)
		os.Exit(1)
	}

	if len(initialInstances) == 0 {
		logger.Error("no instances loaded from source", "url", instancesURL)
		os.Exit(1)
	}

	logger.Info("performing initial health check on instances")
	sortedInstances := checkAndSortInstances(initialInstances)

	if len(sortedInstances) == 0 {
		logger.Error("no instances available after health check")
		os.Exit(1)
	}

	cache := NewLocalCache()

	proxy := &ProxyServer{
		cache:          cache,
		instances:      sortedInstances,
		instanceHealth: make(map[string]*InstanceHealth),
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		config: cfg,
	}

	logger.Info("proxy server initialized",
		"instances", len(sortedInstances),
		"config", cfg)

	proxy.startHealthChecker()

	http.HandleFunc("/", proxy.handleRequest)
	http.HandleFunc("/health", proxy.handleHealth)

	addr := ":" + cfg.Port
	server := &http.Server{
		Addr:    addr,
		Handler: nil,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("HTTP server starting", "address", addr, "port", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err, "address", addr)
			os.Exit(1)
		}
	}()

	sig := <-sigChan
	logger.Info("shutdown signal received", "signal", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logger.Info("initiating graceful shutdown", "timeout", 30*time.Second)
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	logger.Info("server stopped successfully")
}
