package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const instancesURL = "https://github.com/EduardPrigoana/monochrome/raw/refs/heads/main/instances.json"
const cacheTTL = 2 * time.Hour

type Config struct {
	Port string
}

func loadConfig() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return Config{Port: port}
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

func NewLocalCache() *LocalCache {
	lc := &LocalCache{
		data: make(map[string]cacheEntry),
	}
	go lc.cleanup()
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
	cache       Cache
	instanceURL string
	client      *http.Client
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
	if path == "/" || path == "/health" {
		return true
	}
	allowedPrefixes := []string{
		"/track/", "/dash/", "/search/", "/cover/", "/song/",
		"/album/", "/playlist/", "/artist/", "/lyrics/", "/home/",
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

	cached, _ := p.cache.Get(ctx, cacheKey)
	if cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Write(cached)
		return
	}

	url := strings.TrimSuffix(p.instanceURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	resp, err := p.client.Do(req)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	p.cache.Set(ctx, cacheKey, body, cacheTTL)

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func main() {
	cfg := loadConfig()

	instances, err := loadInstances(instancesURL)
	if err != nil {
		log.Fatalf("Failed to load instances: %v", err)
	}
	if len(instances) == 0 {
		log.Fatal("No instances found in the list.")
	}
	firstInstance := instances[0]
	log.Printf("Using instance: %s", firstInstance)

	proxy := &ProxyServer{
		cache:       NewLocalCache(),
		instanceURL: firstInstance,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	http.HandleFunc("/", proxy.handleRequest)
	addr := ":" + cfg.Port
	log.Printf("Proxy server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}