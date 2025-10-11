package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const instancesURL = "https://github.com/EduardPrigoana/monochrome/raw/refs/heads/main/instances.json"

type Config struct {
	Port          string
	RedisURL      string
	RedisPassword string
}

type Cache interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte, ttl time.Duration) error
}

type RedisCache struct {
	client *redis.Client
	ctx    context.Context
}

func (r *RedisCache) Get(key string) ([]byte, error) {
	val, err := r.client.Get(r.ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(val), nil
}

func (r *RedisCache) Set(key string, value []byte, ttl time.Duration) error {
	return r.client.Set(r.ctx, key, value, ttl).Err()
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

func (l *LocalCache) Get(key string) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	entry, exists := l.data[key]
	if !exists {
		return nil, nil
	}

	if time.Now().After(entry.expiration) {
		return nil, nil
	}

	return entry.value, nil
}

func (l *LocalCache) Set(key string, value []byte, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.data[key] = cacheEntry{
		value:      value,
		expiration: time.Now().Add(ttl),
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
			if now.After(entry.expiration) {
				delete(l.data, key)
			}
		}
		l.mu.Unlock()
	}
}

type ProxyServer struct {
	cache     Cache
	instances []string
	client    *http.Client
}

func loadConfig() Config {
	return Config{
		Port:          getEnv("PORT", "8080"),
		RedisURL:      os.Getenv("REDIS_URL"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func initCache(cfg Config) Cache {
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			fmt.Printf("Failed to parse Redis URL, using local cache: %v\n", err)
			return NewLocalCache()
		}

		if cfg.RedisPassword != "" {
			opt.Password = cfg.RedisPassword
		}

		client := redis.NewClient(opt)
		ctx := context.Background()

		if err := client.Ping(ctx).Err(); err != nil {
			fmt.Printf("Failed to connect to Redis, using local cache: %v\n", err)
			return NewLocalCache()
		}

		fmt.Println("Connected to Redis cache")
		return &RedisCache{client: client, ctx: ctx}
	}

	fmt.Println("Using local cache")
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

func isAllowedPath(path string) bool {
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

func (p *ProxyServer) setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func (p *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
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

	cached, err := p.cache.Get(cacheKey)
	if err != nil {
		fmt.Printf("Cache get error: %v\n", err)
	}

	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Write(cached)
		return
	}

	var lastErr error
	for _, instance := range p.instances {
		url := strings.TrimSuffix(instance, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}

		resp, err := p.client.Get(url)
		if err != nil {
			lastErr = err
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := p.cache.Set(cacheKey, body, 2*time.Hour); err != nil {
				fmt.Printf("Cache set error: %v\n", err)
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

		lastErr = fmt.Errorf("status code: %d", resp.StatusCode)
	}

	if lastErr != nil {
		http.Error(w, fmt.Sprintf("All instances failed: %v", lastErr), http.StatusBadGateway)
	} else {
		http.Error(w, "No instances available", http.StatusServiceUnavailable)
	}
}

func main() {
	cfg := loadConfig()

	instances, err := loadInstances(instancesURL)
	if err != nil {
		fmt.Printf("Failed to load instances: %v\n", err)
		os.Exit(1)
	}

	if len(instances) == 0 {
		fmt.Println("No instances loaded")
		os.Exit(1)
	}

	fmt.Printf("Loaded %d instances\n", len(instances))

	cache := initCache(cfg)

	proxy := &ProxyServer{
		cache:     cache,
		instances: instances,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	http.HandleFunc("/", proxy.handleRequest)

	addr := ":" + cfg.Port
	fmt.Printf("Server starting on %s\n", addr)

	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Printf("Server failed: %v\n", err)
		os.Exit(1)
	}
}
