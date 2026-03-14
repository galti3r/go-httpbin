package httpbin

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// mixDirective represents a single directive in the /mix DSL
type mixDirective struct {
	kind  string // "s" (status), "h" (header), "d" (delay), "b64" (body)
	value string
}

// mixAllowedHeaders is the allowlist of headers that can be set via /mix
var mixAllowedHeaders = map[string]bool{
	"retry-after":              true,
	"cache-control":            true,
	"content-language":         true,
	"x-ratelimit-limit":       true,
	"x-ratelimit-remaining":   true,
	"x-ratelimit-reset":       true,
}

func isAllowedMixHeader(name string) bool {
	// Allow any header starting with X- (custom headers)
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "x-") {
		return true
	}
	return mixAllowedHeaders[lower]
}

// parseMixDirectives parses the path segments after /mix/ into directives
func parseMixDirectives(path string) ([]mixDirective, error) {
	// Remove leading "/mix/" or "/mix"
	path = strings.TrimPrefix(path, "/mix/")
	path = strings.TrimPrefix(path, "/mix")
	if path == "" || path == "/" {
		return nil, nil
	}
	path = strings.TrimPrefix(path, "/")

	segments := strings.Split(path, "/")
	if len(segments) > 20 {
		return nil, fmt.Errorf("too many directives (max 20, got %d)", len(segments))
	}

	var directives []mixDirective
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		key, value, found := strings.Cut(seg, "=")
		if !found {
			return nil, fmt.Errorf("invalid directive %q: must be key=value", seg)
		}
		switch key {
		case "s", "h", "d", "b64":
			directives = append(directives, mixDirective{kind: key, value: value})
		default:
			return nil, fmt.Errorf("unknown directive %q: must be s, h, d, or b64", key)
		}
	}
	return directives, nil
}

// Mix handles the /mix endpoint with composable DSL
func (h *HTTPBin) Mix(w http.ResponseWriter, r *http.Request) {
	directives, err := parseMixDirectives(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	status := http.StatusOK
	var body []byte
	var totalDelay time.Duration

	for _, d := range directives {
		switch d.kind {
		case "s":
			code, err := parseStatusCode(d.value)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid status in /mix: %w", err))
				return
			}
			status = code

		case "h":
			name, val, found := strings.Cut(d.value, ":")
			if !found {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid header directive %q: must be name:value", d.value))
				return
			}
			// Strip CRLF for safety
			val = strings.NewReplacer("\r", "", "\n", "").Replace(val)
			name = strings.NewReplacer("\r", "", "\n", "").Replace(name)
			if !isAllowedMixHeader(name) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("header %q is not allowed in /mix", name))
				return
			}
			w.Header().Set(name, val)

		case "d":
			delay, err := parseBoundedDuration(d.value, 0, h.MaxDuration)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid delay in /mix: %w", err))
				return
			}
			totalDelay += delay

		case "b64":
			decoded, err := base64.URLEncoding.DecodeString(d.value)
			if err != nil {
				// Try standard encoding
				decoded, err = base64.StdEncoding.DecodeString(d.value)
				if err != nil {
					writeError(w, http.StatusBadRequest, fmt.Errorf("invalid base64 body in /mix: %w", err))
					return
				}
			}
			body = decoded
		}
	}

	// Enforce total delay budget
	if totalDelay > h.MaxDuration {
		writeError(w, http.StatusBadRequest, fmt.Errorf("total delay %s exceeds maximum %s", totalDelay, h.MaxDuration))
		return
	}

	// Apply delay
	if totalDelay > 0 {
		select {
		case <-time.After(totalDelay):
		case <-r.Context().Done():
			return
		}
	}

	if len(body) > 0 {
		w.WriteHeader(status)
		w.Write(body)
	} else {
		w.WriteHeader(status)
	}
}
