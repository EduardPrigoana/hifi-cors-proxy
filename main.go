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

	"github.com/redis/go-redis/v9"
)

const instancesURL = "https://github.com/EduardPrigoana/monochrome/raw/refs/heads/main/instances.json"
const checkTimeout = 10 * time.Second
const maxLatency = checkTimeout

var logger *slog.Logger

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
	mu          sync.RWMutex
}

type Config struct {
	Port                string
	RedisURL            string
	RedisPassword       string
	CacheTTL            time.Duration
	HealthCheckInterval time.Duration
	RequestTimeout      time.Duration
	MaxRetries          int
}

func loadConfig() (Config, error) {
	cfg := Config{
		Port:                getEnv("PORT", "8080"),
		RedisURL:            os.Getenv("REDIS_URL"),
		RedisPassword:       os.Getenv("REDIS_PASSWORD"),
		CacheTTL:            getDurationEnv("CACHE_TTL", 2*time.Hour),
		HealthCheckInterval: getDurationEnv("HEALTH_CHECK_INTERVAL", 30*time.Minute),
		RequestTimeout:      getDurationEnv("REQUEST_TIMEOUT", 30*time.Second),
		MaxRetries:          getIntEnv("MAX_RETRIES", 3),
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if c.CacheTTL < 0 {
		return fmt.Errorf("cache TTL must be positive")
	}
	if c.HealthCheckInterval < time.Minute {
		return fmt.Errorf("health check interval must be at least 1 minute")
	}
	if c.RequestTimeout < time.Second {
		return fmt.Errorf("request timeout must be at least 1 second")
	}
	if c.MaxRetries < 1 {
		return fmt.Errorf("max retries must be at least 1")
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func getIntEnv(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

type RedisCache struct {
	client *redis.Client
}

func (r *RedisCache) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(val), nil
}

func (r *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

type LocalCache struct {
	data map[string]cacheEntry
	mu   sync.RWMutex
}

type cacheEntry struct {
	value      []byte
	expiration time.Time
}

func NewLocalCache() *LocalCache {
	lc := &LocalCache{
		data: make(map[string]cacheEntry),
	}
	go lc.cleanup()
	return lc
}

func (l *LocalCache) Get(ctx context.Context, key string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	entry, exists := l.data[key]
	if !exists {
		return nil, nil
	}

	if !entry.expiration.IsZero() && time.Now().After(entry.expiration) {
		return nil, nil
	}

	return entry.value, nil
}

func (l *LocalCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}

	l.data[key] = cacheEntry{
		value:      value,
		expiration: exp,
	}

	return nil
}

func (l *LocalCache) cleanup() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		l.mu.Lock()
		now := time.Now()
		for key, entry := range l.data {
			if !entry.expiration.IsZero() && now.After(entry.expiration) {
				delete(l.data, key)
			}
		}
		l.mu.Unlock()
	}
}

type ProxyServer struct {
	cache          Cache
	instances      []string
	instanceHealth map[string]*InstanceHealth
	mu             sync.RWMutex
	client         *http.Client
	config         Config
}

func initCache(cfg Config) Cache {
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			logger.Warn("failed to parse Redis URL, using local cache", "error", err)
			return NewLocalCache()
		}

		if cfg.RedisPassword != "" {
			opt.Password = cfg.RedisPassword
		}

		client := redis.NewClient(opt)
		ctx := context.Background()

		if err := client.Ping(ctx).Err(); err != nil {
			logger.Warn("failed to connect to Redis, using local cache", "error", err)
			return NewLocalCache()
		}

		logger.Info("connected to Redis cache")
		return &RedisCache{client: client}
	}

	logger.Info("using local cache")
	return NewLocalCache()
}

func loadInstances(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var instances []string
	if err := json.Unmarshal(body, &instances); err != nil {
		return nil, err
	}

	return instances, nil
}

func checkAndSortInstances(instances []string) []string {
	logger.Info("checking and sorting instances by performance")

	var wg sync.WaitGroup
	performanceResults := make([]InstancePerformance, 0, len(instances))
	var mu sync.Mutex

	client := &http.Client{
		Timeout: checkTimeout,
	}

	for _, instance := range instances {
		wg.Add(1)
		go func(instanceURL string) {
			defer wg.Done()

			testURL := strings.TrimSuffix(instanceURL, "/") + "/search/?s=ilybasement"

			start := time.Now()
			resp, err := client.Get(testURL)
			latency := time.Since(start)

			currentPerf := InstancePerformance{URL: instanceURL}

			if err != nil {
				currentPerf.Latency = maxLatency
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					currentPerf.Latency = latency
				} else {
					currentPerf.Latency = maxLatency
				}
			}

			mu.Lock()
			performanceResults = append(performanceResults, currentPerf)
			mu.Unlock()

		}(instance)
	}

	wg.Wait()

	sort.Slice(performanceResults, func(i, j int) bool {
		return performanceResults[i].Latency < performanceResults[j].Latency
	})

	sortedInstanceURLs := make([]string, 0, len(instances))
	logger.Info("instance performance check complete")
	for i, perf := range performanceResults {
		sortedInstanceURLs = append(sortedInstanceURLs, perf.URL)
		if perf.Latency >= maxLatency {
			logger.Warn("instance check", "rank", i+1, "url", perf.URL, "status", "FAILED or TIMEOUT")
		} else {
			logger.Info("instance check", "rank", i+1, "url", perf.URL, "latency", perf.Latency)
		}
	}

	return sortedInstanceURLs
}

func isAllowedPath(path string) bool {
	if path == "/" || path == "/health" {
		return true
	}

	allowedPrefixes := []string{
		"/track/",
		"/dash/",
		"/search/",
		"/cover/",
		"/song/",
		"/album/",
		"/playlist/",
		"/artist/",
		"/lyrics/",
	}

	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

func (p *ProxyServer) getInstances() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string{}, p.instances...)
}

func (p *ProxyServer) updateInstances(instances []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instances = instances
}

func (p *ProxyServer) startHealthChecker() {
	ticker := time.NewTicker(p.config.HealthCheckInterval)
	go func() {
		for range ticker.C {
			logger.Info("starting periodic health check")
			instances := p.getInstances()
			sortedInstances := checkAndSortInstances(instances)
			p.updateInstances(sortedInstances)
		}
	}()
}

func (p *ProxyServer) isHealthy(instance string) bool {
	p.mu.RLock()
	health := p.instanceHealth[instance]
	p.mu.RUnlock()

	if health == nil {
		return true
	}

	health.mu.RLock()
	defer health.mu.RUnlock()

	if health.failures >= 3 && time.Since(health.lastFailure) < 5*time.Minute {
		return false
	}

	return true
}

func (p *ProxyServer) recordFailure(instance string) {
	p.mu.Lock()
	if p.instanceHealth[instance] == nil {
		p.instanceHealth[instance] = &InstanceHealth{}
	}
	health := p.instanceHealth[instance]
	p.mu.Unlock()

	health.mu.Lock()
	health.failures++
	health.lastFailure = time.Now()
	health.mu.Unlock()

	logger.Warn("instance failure recorded", "instance", instance, "failures", health.failures)
}

func (p *ProxyServer) recordSuccess(instance string) {
	p.mu.RLock()
	health := p.instanceHealth[instance]
	p.mu.RUnlock()

	if health != nil {
		health.mu.Lock()
		if health.failures > 0 {
			logger.Info("instance recovered", "instance", instance)
			health.failures = 0
		}
		health.mu.Unlock()
	}
}

func (p *ProxyServer) fetchWithRetry(ctx context.Context, url string) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i < p.config.MaxRetries; i++ {
		req, reqErr := http.NewRequestWithContext(ctx, "GET", url, nil)
		if reqErr != nil {
			return nil, reqErr
		}

		resp, err = p.client.Do(req)

		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}

		if resp != nil {
			resp.Body.Close()
		}

		if i < p.config.MaxRetries-1 {
			backoff := time.Duration(1<<uint(i)) * time.Second
			logger.Debug("retrying request", "url", url, "attempt", i+1, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return nil, err
}

func (p *ProxyServer) setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func (p *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p.setCORSHeaders(w)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !isAllowedPath(r.URL.Path) {
		http.Error(w, "Path not allowed", http.StatusForbidden)
		return
	}

	cacheKey := r.URL.Path
	if r.URL.RawQuery != "" {
		cacheKey += "?" + r.URL.RawQuery
	}

	cached, err := p.cache.Get(ctx, cacheKey)
	if err != nil {
		logger.Error("cache get error", "error", err, "key", cacheKey)
	}

	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Write(cached)
		return
	}

	var lastErr error
	instances := p.getInstances()

	for _, instance := range instances {
		if !p.isHealthy(instance) {
			logger.Debug("skipping unhealthy instance", "instance", instance)
			continue
		}

		url := strings.TrimSuffix(instance, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}

		resp, err := p.fetchWithRetry(ctx, url)
		if err != nil {
			lastErr = err
			p.recordFailure(instance)
			logger.Warn("instance request failed", "instance", instance, "error", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = err
			p.recordFailure(instance)
			logger.Warn("failed to read response body", "instance", instance, "error", err)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.recordSuccess(instance)

			if err := p.cache.Set(ctx, cacheKey, body, p.config.CacheTTL); err != nil {
				logger.Error("cache set error", "error", err, "key", cacheKey)
			}

			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}

		p.recordFailure(instance)
		lastErr = &ProxyError{
			Code:    resp.StatusCode,
			Message: "non-2xx status code from instance",
			Err:     fmt.Errorf("status code: %d", resp.StatusCode),
		}
	}

	if lastErr != nil {
		logger.Error("all instances failed", "error", lastErr)
		http.Error(w, fmt.Sprintf("All instances failed: %v", lastErr), http.StatusBadGateway)
	} else {
		logger.Error("no instances available?")
		http.Error(w, "No instances available", http.StatusServiceUnavailable)
	}
}

func (p *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	instances := p.getInstances()
	healthyCount := 0

	for _, instance := range instances {
		if p.isHealthy(instance) {
			healthyCount++
		}
	}

	status := map[string]interface{}{
		"status":            "ok",
		"total_instances":   len(instances),
		"healthy_instances": healthyCount,
	}

	json.NewEncoder(w).Encode(status)
}

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	initialInstances, err := loadInstances(instancesURL)
	if err != nil {
		logger.Error("failed to load instances", "error", err)
		os.Exit(1)
	}

	if len(initialInstances) == 0 {
		logger.Error("no instances loaded from source")
		os.Exit(1)
	}

	logger.Info("loaded instances from source", "count", len(initialInstances))

	sortedInstances := checkAndSortInstances(initialInstances)

	if len(sortedInstances) == 0 {
		logger.Error("no instances available after health check")
		os.Exit(1)
	}

	cache := initCache(cfg)

	proxy := &ProxyServer{
		cache:          cache,
		instances:      sortedInstances,
		instanceHealth: make(map[string]*InstanceHealth),
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		config: cfg,
	}

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
		logger.Info("server starting", "port", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-sigChan
	logger.Info("shutting down gracefully")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	logger.Info("server stopped")
}
