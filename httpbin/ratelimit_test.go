package httpbin

import (
	"testing"
	"time"

	"github.com/galti3r/go-httpbin/v3/internal/testing/assert"
)

func TestTokenBucket(t *testing.T) {
	t.Parallel()

	t.Run("basic_allow", func(t *testing.T) {
		t.Parallel()
		b := &tokenBucket{
			tokens:     5,
			maxTokens:  5,
			refillRate: 1,
			lastRefill: time.Now(),
		}
		for i := 0; i < 5; i++ {
			assert.Equal(t, b.allow(time.Now()), true, "should allow request")
		}
		assert.Equal(t, b.allow(time.Now()), false, "should deny request after burst")
	})

	t.Run("refill", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		b := &tokenBucket{
			tokens:     0,
			maxTokens:  5,
			refillRate: 10,
			lastRefill: now,
		}
		assert.Equal(t, b.allow(now), false, "should deny when empty")
		// After 1 second, should have 10 tokens (capped at 5)
		assert.Equal(t, b.allow(now.Add(1*time.Second)), true, "should allow after refill")
	})
}

func TestIPRateLimiter(t *testing.T) {
	t.Parallel()

	t.Run("per_ip", func(t *testing.T) {
		t.Parallel()
		rl := newIPRateLimiter(RateLimiterConfig{
			Rate:          100,
			Burst:         2,
			MaxTrackedIPs: 100,
			EntryLifetime: 5 * time.Minute,
		})
		assert.Equal(t, rl.allow("1.2.3.4"), true, "first request should be allowed")
		assert.Equal(t, rl.allow("1.2.3.4"), true, "second request should be allowed")
		assert.Equal(t, rl.allow("1.2.3.4"), false, "third request should be denied")
		// Different IP should have its own bucket
		assert.Equal(t, rl.allow("5.6.7.8"), true, "different IP should be allowed")
	})
}

func TestNormalizeIPForRateLimit(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		input      string
		useSubnets bool
		want       string
	}{
		"no_subnet":   {"1.2.3.4", false, "1.2.3.4"},
		"ipv4_subnet": {"1.2.3.4", true, "1.2.3.0/24"},
		"ipv6_subnet": {"2001:db8:1:2:3:4:5:6", true, "2001:db8:1:2::/64"},
		"invalid_ip":  {"not-an-ip", true, "not-an-ip"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := normalizeIPForRateLimit(tc.input, tc.useSubnets)
			assert.Equal(t, got, tc.want, "wrong normalized IP")
		})
	}
}
