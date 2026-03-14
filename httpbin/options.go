package httpbin

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// OptionFunc uses the "functional options" pattern to customize an HTTPBin
// instance
type OptionFunc func(*HTTPBin)

// WithDefaultParams sets the default params handlers will use
func WithDefaultParams(defaultParams DefaultParams) OptionFunc {
	return func(h *HTTPBin) {
		h.DefaultParams = defaultParams
	}
}

// WithMaxBodySize sets the maximum amount of memory
func WithMaxBodySize(m int64) OptionFunc {
	return func(h *HTTPBin) {
		h.MaxBodySize = m
	}
}

// WithMaxDuration sets the maximum amount of time httpbin may take to respond
func WithMaxDuration(d time.Duration) OptionFunc {
	return func(h *HTTPBin) {
		h.MaxDuration = d
	}
}

// WithHostname sets the hostname to return via the /hostname endpoint.
func WithHostname(s string) OptionFunc {
	return func(h *HTTPBin) {
		h.hostname = s
	}
}

// WithObserver sets the request observer callback
func WithObserver(o Observer) OptionFunc {
	return func(h *HTTPBin) {
		h.Observer = o
	}
}

// WithEnv sets the HTTPBIN_-prefixed environment variables reported
// by the /env endpoint.
func WithEnv(env map[string]string) OptionFunc {
	return func(h *HTTPBin) {
		h.env = env
	}
}

// WithExcludeHeaders sets the headers to exclude in outgoing responses, to
// prevent possible information leakage.
func WithExcludeHeaders(excludeHeaders string) OptionFunc {
	return func(h *HTTPBin) {
		h.setExcludeHeaders(excludeHeaders)
	}
}

// WithPrefix sets the path prefix
func WithPrefix(p string) OptionFunc {
	return func(h *HTTPBin) {
		h.prefix = p
	}
}

// WithAllowedRedirectDomains limits the domains to which the /redirect-to
// endpoint will redirect traffic.
func WithAllowedRedirectDomains(hosts []string) OptionFunc {
	return func(h *HTTPBin) {
		hostSet := make(map[string]struct{}, len(hosts))
		formattedListItems := make([]string, 0, len(hosts))
		for _, host := range hosts {
			hostSet[host] = struct{}{}
			formattedListItems = append(formattedListItems, fmt.Sprintf("- %s", host))
		}
		h.AllowedRedirectDomains = hostSet

		sort.Strings(formattedListItems)
		h.forbiddenRedirectError = fmt.Sprintf(`Forbidden redirect URL. Please be careful with this link.

Allowed redirect destinations:
%s`, strings.Join(formattedListItems, "\n"))
	}
}

// WithUnsafeAllowDangerousResponses means endpoints that allow clients to
// specify a response Conntent-Type WILL NOT escape HTML entities in the
// response body, which can enable (e.g.) reflected XSS attacks.
//
// This configuration is only supported for backwards compatibility if
// absolutely necessary.
func WithUnsafeAllowDangerousResponses() OptionFunc {
	return func(h *HTTPBin) {
		h.unsafeAllowDangerousResponses = true
	}
}

// WithTrustedProxies configures trusted proxy CIDRs for X-Forwarded-For
// header parsing. If not called, all proxy headers are trusted (backward
// compatible). Pass nil to trust no proxy headers (RemoteAddr only).
func WithTrustedProxies(cidrs []*net.IPNet) OptionFunc {
	return func(h *HTTPBin) {
		h.trustedProxies = cidrs
		h.trustedProxiesConfigured = true
	}
}

// WithVersion sets the version string returned by the /version endpoint.
func WithVersion(v string) OptionFunc {
	return func(h *HTTPBin) {
		h.version = v
	}
}

// WithRateLimiter configures the per-IP rate limiter.
func WithRateLimiter(cfg RateLimiterConfig) OptionFunc {
	return func(h *HTTPBin) {
		h.rateLimiter = newIPRateLimiter(cfg)
		h.rateLimitUseSubnets = cfg.UseSubnets
	}
}

// WithMaxConcurrentRequests sets the maximum number of concurrent requests.
func WithMaxConcurrentRequests(n int) OptionFunc {
	return func(h *HTTPBin) {
		h.maxConcurrentRequests = n
	}
}

// ParseTrustedProxies parses a comma-separated list of CIDRs into a slice
// of *net.IPNet. The special value "none" returns an empty slice (trust no
// proxy headers). An empty string returns nil (trust all, backward compat).
func ParseTrustedProxies(raw string) ([]*net.IPNet, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false, nil
	}
	if strings.EqualFold(raw, "none") {
		return []*net.IPNet{}, true, nil
	}

	var cidrs []*net.IPNet
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// If no mask provided, add one
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			return nil, false, fmt.Errorf("invalid CIDR %q: %w", s, err)
		}
		cidrs = append(cidrs, cidr)
	}
	return cidrs, true, nil
}
