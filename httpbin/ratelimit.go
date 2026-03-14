package httpbin

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimiterConfig holds configuration for the per-IP rate limiter.
type RateLimiterConfig struct {
	Rate            float64       // requests per second per IP
	Burst           int           // max burst size
	MaxTrackedIPs   int           // max tracked IP entries
	EntryLifetime   time.Duration // TTL for idle entries
	CleanupInterval time.Duration // background cleanup interval
	UseSubnets      bool          // group by /24 (IPv4) or /64 (IPv6)
}

type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64
	lastRefill time.Time
}

func (b *tokenBucket) allow(now time.Time) bool {
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type rateLimitEntry struct {
	bucket   tokenBucket
	lastSeen time.Time
}

type ipRateLimiter struct {
	mu            sync.Mutex
	rate          float64
	burst         float64
	buckets       map[string]*rateLimitEntry
	maxEntries    int
	entryLifetime time.Duration
	stopCleanup   func()
}

func newIPRateLimiter(cfg RateLimiterConfig) *ipRateLimiter {
	rl := &ipRateLimiter{
		rate:          cfg.Rate,
		burst:         float64(cfg.Burst),
		buckets:       make(map[string]*rateLimitEntry),
		maxEntries:    cfg.MaxTrackedIPs,
		entryLifetime: cfg.EntryLifetime,
	}
	if cfg.CleanupInterval > 0 {
		rl.stopCleanup = rl.startCleanup(cfg.CleanupInterval)
	}
	return rl
}

func (rl *ipRateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	entry, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rl.maxEntries {
			rl.evictExpiredLocked(now)
		}
		if len(rl.buckets) >= rl.maxEntries {
			rl.evictOldestLocked()
		}
		rl.buckets[key] = &rateLimitEntry{
			bucket: tokenBucket{
				tokens:     rl.burst - 1,
				maxTokens:  rl.burst,
				refillRate: rl.rate,
				lastRefill: now,
			},
			lastSeen: now,
		}
		return true
	}
	entry.lastSeen = now
	return entry.bucket.allow(now)
}

func (rl *ipRateLimiter) evictExpiredLocked(now time.Time) {
	cutoff := now.Add(-rl.entryLifetime)
	for key, entry := range rl.buckets {
		if entry.lastSeen.Before(cutoff) {
			delete(rl.buckets, key)
		}
	}
}

func (rl *ipRateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for key, entry := range rl.buckets {
		if first || entry.lastSeen.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.lastSeen
			first = false
		}
	}
	if oldestKey != "" {
		delete(rl.buckets, oldestKey)
	}
}

func (rl *ipRateLimiter) startCleanup(interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.mu.Lock()
				rl.evictExpiredLocked(time.Now())
				rl.mu.Unlock()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// normalizeIPForRateLimit converts an IP to its rate-limiting key.
func normalizeIPForRateLimit(rawIP string, useSubnets bool) string {
	if !useSubnets {
		return rawIP
	}
	ip := net.ParseIP(rawIP)
	if ip == nil {
		return rawIP
	}
	if ip4 := ip.To4(); ip4 != nil {
		masked := ip4.Mask(net.CIDRMask(24, 32))
		return masked.String() + "/24"
	}
	masked := ip.Mask(net.CIDRMask(64, 128))
	return masked.String() + "/64"
}

// rateLimitMiddleware creates rate limiting middleware.
func rateLimitMiddleware(limiter *ipRateLimiter, clientIPFunc func(*http.Request) string, useSubnets bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := clientIPFunc(r)
		key := normalizeIPForRateLimit(clientIP, useSubnets)
		if !limiter.allow(key) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, nil)
			return
		}
		h.ServeHTTP(w, r)
	})
}
