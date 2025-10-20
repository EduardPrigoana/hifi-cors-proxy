package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
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
	instancesURL         = "https://github.com/EduardPrigoana/monochrome/raw/refs/heads/main/instances.json"
	checkTimeout         = 10 * time.Second
	maxLatency           = checkTimeout
	requestIDBytes       = 4
	maxFailures          = 3
	backoffDuration      = 5 * time.Minute
	cacheCleanupInterval = 10 * time.Minute
	initialRetryDelay    = 500 * time.Millisecond
)

var logger *slog.Logger
var defaultUserAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36",
}

type contextKey string

const requestIDKey = contextKey("requestID")

type Config struct {
	Port                string
	LogLevel            slog.Level
	CacheTTL            time.Duration
	HealthCheckInterval time.Duration
	RequestTimeout      time.Duration
	MaxRetries          int
	UserAgents          []string
}

func loadConfig() (Config, error) {
	cfg := Config{
		Port:                getEnv("PORT", "8080"),
		LogLevel:            getLogLevelEnv("LOG_LEVEL", slog.LevelInfo),
		CacheTTL:            getDurationEnv("CACHE_TTL", 1*time.Hour),
		HealthCheckInterval: getDurationEnv("HEALTH_CHECK_INTERVAL", 30*time.Minute),
		RequestTimeout:      getDurationEnv("REQUEST_TIMEOUT", 30*time.Second),
		MaxRetries:          getIntEnv("MAX_RETRIES", 3),
		UserAgents:          parsePipeSVEnv("USER_AGENTS", defaultUserAgents),
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.CacheTTL < 0 {
		return fmt.Errorf("CACHE_TTL must be non-negative")
	}
	if c.HealthCheckInterval < time.Minute {
		return fmt.Errorf("HEALTH_CHECK_INTERVAL must be at least 1 minute")
	}
	if c.RequestTimeout < time.Second {
		return fmt.Errorf("REQUEST_TIMEOUT must be at least 1 second")
	}
	if c.MaxRetries < 1 {
		return fmt.Errorf("MAX_RETRIES must be at least 1")
	}
	if len(c.UserAgents) == 0 {
		return fmt.Errorf("USER_AGENTS cannot be empty")
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

func getLogLevelEnv(key string, defaultValue slog.Level) slog.Level {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(value)); err != nil {
		return defaultValue
	}
	return level
}

func parsePipeSVEnv(key string, defaultSlice []string) []string {
	value := os.Getenv(key)
	if value == "" {
		return defaultSlice
	}
	parts := strings.Split(value, "|")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) > 0 {
		return cleaned
	}
	return defaultSlice
}

type InstanceHealth struct {
	failures    int
	lastFailure time.Time
	mu          sync.RWMutex
}

type InstancePerformance struct {
	URL     string
	Latency time.Duration
}

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

type LocalCache struct {
	data map[string]cacheEntry
	mu   sync.RWMutex
}

type cacheEntry struct {
	value      []byte
	expiration time.Time
}

func NewLocalCache(ctx context.Context) *LocalCache {
	lc := &LocalCache{
		data: make(map[string]cacheEntry),
	}
	go lc.cleanup(ctx)
	return lc
}

func (l *LocalCache) Get(ctx context.Context, key string) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	entry, exists := l.data[key]
	if !exists || (!entry.expiration.IsZero() && time.Now().After(entry.expiration)) {
		return nil, nil
	}
	return entry.value, nil
}

func (l *LocalCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
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

func (l *LocalCache) cleanup(ctx context.Context) {
	ticker := time.NewTicker(cacheCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.mu.Lock()
			now := time.Now()
			for key, entry := range l.data {
				if !entry.expiration.IsZero() && now.After(entry.expiration) {
					delete(l.data, key)
				}
			}
			l.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

type ProxyServer struct {
	cache             Cache
	instances         []string
	instanceHealth    map[string]*InstanceHealth
	mu                sync.RWMutex
	client            *http.Client
	healthCheckClient *http.Client
	config            Config
}

func newProxyServer(ctx context.Context, cfg Config, sortedInstances []string) *ProxyServer {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &ProxyServer{
		cache:          NewLocalCache(ctx),
		instances:      sortedInstances,
		instanceHealth: make(map[string]*InstanceHealth),
		client: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		healthCheckClient: &http.Client{
			Timeout:   checkTimeout,
			Transport: transport,
		},
		config: cfg,
	}
}

func (p *ProxyServer) getRandomUserAgent() string {
	if len(p.config.UserAgents) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(p.config.UserAgents))))
	if err != nil {
		return p.config.UserAgents[0]
	}
	return p.config.UserAgents[n.Int64()]
}

func loadInstances(ctx context.Context, url string) ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch instances, status: %s", resp.Status)
	}
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

func checkAndSortInstances(ctx context.Context, instances []string, client *http.Client) []string {
	logger.Info("Checking and sorting instances by performance")
	var wg sync.WaitGroup
	performanceResults := make(chan InstancePerformance, len(instances))
	for _, instance := range instances {
		wg.Add(1)
		go func(instanceURL string) {
			defer wg.Done()
			testURL := strings.TrimSuffix(instanceURL, "/") + "/search/?s=ilybasement"
			currentPerf := InstancePerformance{URL: instanceURL, Latency: maxLatency}
			defer func() { performanceResults <- currentPerf }()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
			if err != nil {
				logger.Warn("Instance check failed to create request", "url", instanceURL, "error", err)
				return
			}
			start := time.Now()
			resp, err := client.Do(req)
			latency := time.Since(start)
			if err != nil {
				logger.Warn("Instance check failed", "url", instanceURL, "error", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				currentPerf.Latency = latency
			} else {
				logger.Warn("Instance check returned non-2xx status", "url", instanceURL, "status", resp.StatusCode)
			}
		}(instance)
	}
	wg.Wait()
	close(performanceResults)
	results := make([]InstancePerformance, 0, len(instances))
	for perf := range performanceResults {
		results = append(results, perf)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Latency < results[j].Latency
	})
	sortedInstanceURLs := make([]string, 0, len(results))
	logger.Info("Instance performance check complete")
	for i, perf := range results {
		sortedInstanceURLs = append(sortedInstanceURLs, perf.URL)
		logAttrs := []any{"rank", i + 1, "url", perf.URL}
		if perf.Latency >= maxLatency {
			logAttrs = append(logAttrs, "status", "UNHEALTHY/TIMEOUT")
			logger.Warn("Instance check result", logAttrs...)
		} else {
			logAttrs = append(logAttrs, "latency_ms", perf.Latency.Milliseconds())
			logger.Info("Instance check result", logAttrs...)
		}
	}
	return sortedInstanceURLs
}

func isAllowedPath(path string) bool {
	if path == "/" || path == "/health" {
		return true
	}
	allowedPrefixes := []string{
		"/track/", "/dash/", "/search/", "/cover/", "/song/", "/album/",
		"/playlist/", "/artist/", "/lyrics/", "/home/", "/mix/",
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
	instancesCopy := make([]string, len(p.instances))
	copy(instancesCopy, p.instances)
	return instancesCopy
}

func (p *ProxyServer) updateInstances(instances []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instances = instances
}

func (p *ProxyServer) startHealthChecker(ctx context.Context) {
	logger.Info("Starting periodic health checker", "interval", p.config.HealthCheckInterval)
	ticker := time.NewTicker(p.config.HealthCheckInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				currentInstances := p.getInstances()
				sortedInstances := checkAndSortInstances(ctx, currentInstances, p.healthCheckClient)
				p.updateInstances(sortedInstances)
			case <-ctx.Done():
				logger.Info("Stopping periodic health checker.")
				return
			}
		}
	}()
}

func (p *ProxyServer) isHealthy(instance string) bool {
	p.mu.RLock()
	health, ok := p.instanceHealth[instance]
	p.mu.RUnlock()
	if !ok {
		return true
	}
	health.mu.RLock()
	defer health.mu.RUnlock()
	return health.failures < maxFailures || time.Since(health.lastFailure) > backoffDuration
}

func (p *ProxyServer) recordFailure(instance string) {
	p.mu.Lock()
	if _, ok := p.instanceHealth[instance]; !ok {
		p.instanceHealth[instance] = &InstanceHealth{}
	}
	health := p.instanceHealth[instance]
	p.mu.Unlock()
	health.mu.Lock()
	health.failures++
	health.lastFailure = time.Now()
	failures := health.failures
	health.mu.Unlock()
	logger.Warn("Instance failure recorded", "instance", instance, "failures", failures)
}

func (p *ProxyServer) recordSuccess(instance string) {
	p.mu.RLock()
	health, ok := p.instanceHealth[instance]
	p.mu.RUnlock()
	if ok {
		health.mu.Lock()
		if health.failures > 0 {
			logger.Info("Instance has recovered", "instance", instance)
			health.failures = 0
		}
		health.mu.Unlock()
	}
}

func shouldRetry(statusCode int, err error) bool {
	if err != nil {
		return true
	}
	return statusCode >= http.StatusInternalServerError || statusCode == http.StatusTooManyRequests
}

// isAuthError checks if the response body contains authentication/token errors
func isAuthError(body []byte) bool {
	bodyStr := string(body)
	
	// Check for common auth error patterns
	authErrorPatterns := []string{
		"invalid payload",
		"Token has invalid payload",
		"invalid token",
		"authentication failed",
		"unauthorized",
		"token expired",
		"invalid credentials",
		"subStatus\":11002",
		"subStatus\":11001",
		"subStatus\":11003",
	}
	
	bodyLower := strings.ToLower(bodyStr)
	for _, pattern := range authErrorPatterns {
		if strings.Contains(bodyLower, strings.ToLower(pattern)) {
			return true
		}
	}
	
	// Also check if it's a JSON response with status 401 and error-like structure
	var jsonResp map[string]interface{}
	if err := json.Unmarshal(body, &jsonResp); err == nil {
		if status, ok := jsonResp["status"].(float64); ok {
			if int(status) == 401 {
				return true
			}
		}
		// Check for error messages in common fields
		for _, field := range []string{"error", "userMessage", "message", "errorMessage"} {
			if msg, ok := jsonResp[field].(string); ok {
				msgLower := strings.ToLower(msg)
				if strings.Contains(msgLower, "token") || 
				   strings.Contains(msgLower, "auth") || 
				   strings.Contains(msgLower, "unauthorized") ||
				   strings.Contains(msgLower, "invalid payload") {
					return true
				}
			}
		}
	}
	
	return false
}

func (p *ProxyServer) fetchWithRetry(ctx context.Context, url string) (*http.Response, error) {
	var lastErr error
	for i := 0; i < p.config.MaxRetries; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", p.getRandomUserAgent())

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
			if i < p.config.MaxRetries-1 {
				backoff := time.Duration(1<<uint(i)) * initialRetryDelay
				reqID, _ := ctx.Value(requestIDKey).(string)
				logger.Debug("Retrying request after network error", "url", url, "attempt", i+2, "backoff_ms", backoff.Milliseconds(), "request_id", reqID, "error", err)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			continue
		}

		if !shouldRetry(resp.StatusCode, nil) {
			return resp, nil
		}

		resp.Body.Close()
		lastErr = fmt.Errorf("server error: status %d", resp.StatusCode)

		if i < p.config.MaxRetries-1 {
			backoff := time.Duration(1<<uint(i)) * initialRetryDelay
			reqID, _ := ctx.Value(requestIDKey).(string)
			logger.Debug("Retrying request after server error", "url", url, "status", resp.StatusCode, "attempt", i+2, "backoff_ms", backoff.Milliseconds(), "request_id", reqID)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("request failed after %d retries: %w", p.config.MaxRetries, lastErr)
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func generateCacheKey(r *http.Request) string {
	return fmt.Sprintf("%s:%s", r.Method, r.URL.String())
}

func copyResponseHeaders(dst http.Header, src http.Header) {
	sensitiveHeaders := map[string]bool{
		"Set-Cookie":    true,
		"Authorization": true,
	}
	for key, values := range src {
		if !sensitiveHeaders[key] {
			dst[key] = values
		}
	}
}

func (p *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID, _ := ctx.Value(requestIDKey).(string)
	setCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if !isAllowedPath(r.URL.Path) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	cacheKey := generateCacheKey(r)
	cached, err := p.cache.Get(ctx, cacheKey)
	if err != nil {
		logger.Error("Cache get error", "error", err, "key", cacheKey, "request_id", reqID)
	}
	if cached != nil {
		// Check if cached response is an auth error
		if !isAuthError(cached) {
			logger.Debug("Cache hit", "key", cacheKey, "request_id", reqID)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			return
		} else {
			// Remove auth error from cache
			logger.Warn("Cached auth error detected, invalidating cache", "key", cacheKey, "request_id", reqID)
			// We'll just skip it and fetch from instances
		}
	}

	var lastErr error
	var healthyInstances, unhealthyInstances []string
	for _, inst := range p.getInstances() {
		if p.isHealthy(inst) {
			healthyInstances = append(healthyInstances, inst)
		} else {
			unhealthyInstances = append(unhealthyInstances, inst)
		}
	}

	allInstances := append(healthyInstances, unhealthyInstances...)
	if len(healthyInstances) < len(allInstances) && len(healthyInstances) > 0 {
		logger.Warn("Some instances are unhealthy, trying healthy ones first", "request_id", reqID, "healthy_count", len(healthyInstances), "unhealthy_count", len(unhealthyInstances))
	}

	for i, instance := range allInstances {
		isFallback := i >= len(healthyInstances)
		if isFallback && i == len(healthyInstances) {
			logger.Warn("All healthy instances failed, falling back to unhealthy ones", "request_id", reqID)
		}

		targetURL := strings.TrimSuffix(instance, "/") + r.URL.String()
		resp, err := p.fetchWithRetry(ctx, targetURL)
		if err != nil {
			lastErr = err
			p.recordFailure(instance)
			logger.Warn("Instance request failed", "instance", instance, "error", err, "request_id", reqID)
			continue
		}
		
		if resp.StatusCode < http.StatusInternalServerError {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = err
				logger.Error("Failed to read response body", "instance", instance, "error", err, "request_id", reqID)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			
			// Check if the response contains auth errors
			if isAuthError(body) {
				logger.Warn("Instance returned auth error, skipping to next instance", 
					"instance", instance, 
					"status", resp.StatusCode, 
					"request_id", reqID)
				p.recordFailure(instance)
				lastErr = fmt.Errorf("authentication error from instance")
				continue
			}
			
			// Valid response - record success
			p.recordSuccess(instance)
			
			// Only cache successful responses (2xx status codes)
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				ttl := p.config.CacheTTL
				if r.URL.Path == "/" {
					ttl = 0
				}
				if err := p.cache.Set(ctx, cacheKey, body, ttl); err != nil {
					logger.Error("Cache set error", "error", err, "key", cacheKey, "request_id", reqID)
				}
			}
			
			copyResponseHeaders(w.Header(), resp.Header)
			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("X-Served-By", instance)
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
		
		resp.Body.Close()
		p.recordFailure(instance)
		lastErr = fmt.Errorf("instance returned status %d", resp.StatusCode)
		logger.Warn("Instance returned server error", "instance", instance, "status", resp.StatusCode, "request_id", reqID)
	}

	if lastErr != nil {
		logger.Error("All instances failed to serve the request", "last_error", lastErr, "request_id", reqID)
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	} else {
		logger.Error("No instances available to handle the request", "request_id", reqID)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	}
}

func (p *ProxyServer) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
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

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := generateRequestID()
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		start := time.Now()
		logger.Info("Request received",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
			"request_id", reqID,
		)
		rw := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r.WithContext(ctx))
		logger.Info("Request completed",
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.status == 0 {
		rw.status = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	return rw.ResponseWriter.Write(b)
}

func generateRequestID() string {
	buf := make([]byte, requestIDBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Default().Error("Failed to load config", "error", err)
		os.Exit(1)
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()
	initialInstances, err := loadInstances(appCtx, instancesURL)
	if err != nil {
		logger.Error("Failed to load initial instances", "error", err)
		os.Exit(1)
	}
	if len(initialInstances) == 0 {
		logger.Error("No instances loaded from source, cannot start")
		os.Exit(1)
	}
	logger.Info("Loaded instances from source", "count", len(initialInstances))
	initialCheckClient := &http.Client{Timeout: checkTimeout}
	sortedInstances := checkAndSortInstances(appCtx, initialInstances, initialCheckClient)
	if len(sortedInstances) == 0 {
		logger.Error("No instances available after initial health check, cannot start")
		os.Exit(1)
	}
	proxy := newProxyServer(appCtx, cfg, sortedInstances)
	proxy.startHealthChecker(appCtx)
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.handleRequest)
	mux.HandleFunc("/health", proxy.handleHealthCheck)
	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: loggingMiddleware(mux),
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		logger.Info("Server starting", "port", cfg.Port, "log_level", cfg.LogLevel.String())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server failed to start", "error", err)
			os.Exit(1)
		}
	}()
	<-sigChan
	logger.Info("Shutdown signal received, starting graceful shutdown")
	cancelApp()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	logger.Info("Server stopped gracefully")
}
