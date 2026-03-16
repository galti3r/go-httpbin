package httpbin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/galti3r/go-httpbin/v3/internal/testing/assert"
	"github.com/galti3r/go-httpbin/v3/internal/testing/must"
)

func TestParsePipeline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		modifiers []pipelineStep
		terminal  pipelineStep
		wantErr   bool
	}{
		{
			name:      "delay+status",
			input:     "/delay/1/status/418",
			modifiers: []pipelineStep{{name: "delay", args: []string{"1"}}},
			terminal:  pipelineStep{name: "status", args: []string{"418"}},
		},
		{
			name:      "response_delay+get",
			input:     "/response_delay/2-4/get",
			modifiers: []pipelineStep{{name: "response_delay", args: []string{"2-4"}}},
			terminal:  pipelineStep{name: "get", args: nil},
		},
		{
			name:  "two_modifiers+image",
			input: "/delay/1/response_delay/2/image/png",
			modifiers: []pipelineStep{
				{name: "delay", args: []string{"1"}},
				{name: "response_delay", args: []string{"2"}},
			},
			terminal: pipelineStep{name: "image", args: []string{"png"}},
		},
		{
			name:      "image_vanity_size",
			input:     "/image/size/large/photo.png",
			modifiers: nil,
			terminal:  pipelineStep{name: "image", args: []string{"size", "large", "photo.png"}},
		},
		{
			name:      "redirect+image",
			input:     "/redirect/3/image/photo.png",
			modifiers: nil,
			terminal:  pipelineStep{name: "redirect", args: []string{"3", "image", "photo.png"}},
		},
		{
			name:      "longest_prefix_cookies_set",
			input:     "/delay/1/cookies/set",
			modifiers: []pipelineStep{{name: "delay", args: []string{"1"}}},
			terminal:  pipelineStep{name: "cookies/set", args: nil},
		},
		{
			name:      "longest_prefix_encoding_utf8",
			input:     "/delay/1/encoding/utf8",
			modifiers: []pipelineStep{{name: "delay", args: []string{"1"}}},
			terminal:  pipelineStep{name: "encoding/utf8", args: nil},
		},
		{
			name:      "basic_auth",
			input:     "/delay/1/basic-auth/user/pass",
			modifiers: []pipelineStep{{name: "delay", args: []string{"1"}}},
			terminal:  pipelineStep{name: "basic-auth", args: []string{"user", "pass"}},
		},
		{
			name:  "double_modifier",
			input: "/delay/1/delay/2/status/200",
			modifiers: []pipelineStep{
				{name: "delay", args: []string{"1"}},
				{name: "delay", args: []string{"2"}},
			},
			terminal: pipelineStep{name: "status", args: []string{"200"}},
		},
		{
			name:  "double_response_delay",
			input: "/response_delay/0/response_delay/0/get",
			modifiers: []pipelineStep{
				{name: "response_delay", args: []string{"0"}},
				{name: "response_delay", args: []string{"0"}},
			},
			terminal: pipelineStep{name: "get", args: nil},
		},
		{
			name:      "empty_segments_cleaned",
			input:     "///delay///1///get///",
			modifiers: []pipelineStep{{name: "delay", args: []string{"1"}}},
			terminal:  pipelineStep{name: "get", args: nil},
		},
		{
			name:      "no_modifier_terminal_only",
			input:     "/status/200",
			modifiers: nil,
			terminal:  pipelineStep{name: "status", args: []string{"200"}},
		},

		// Dual-role status: terminal when last
		{
			name:      "status_terminal_alone",
			input:     "/status/418",
			modifiers: nil,
			terminal:  pipelineStep{name: "status", args: []string{"418"}},
		},
		// Dual-role status: modifier when followed by more
		{
			name:      "status_modifier_with_body",
			input:     "/status/422/body/SGVsbG8=",
			modifiers: []pipelineStep{{name: "status", args: []string{"422"}}},
			terminal:  pipelineStep{name: "body", args: []string{"SGVsbG8="}},
		},
		{
			name:  "delay_status_body",
			input: "/delay/1/status/422/body/SGVsbG8=",
			modifiers: []pipelineStep{
				{name: "delay", args: []string{"1"}},
				{name: "status", args: []string{"422"}},
			},
			terminal: pipelineStep{name: "body", args: []string{"SGVsbG8="}},
		},
		{
			name:  "status_modifier_get",
			input: "/delay/1/status/422/get",
			modifiers: []pipelineStep{
				{name: "delay", args: []string{"1"}},
				{name: "status", args: []string{"422"}},
			},
			terminal: pipelineStep{name: "get", args: nil},
		},
		{
			name:      "header_status",
			input:     "/header/X-Custom:test/status/200",
			modifiers: []pipelineStep{{name: "header", args: []string{"X-Custom:test"}}},
			terminal:  pipelineStep{name: "status", args: []string{"200"}},
		},
		{
			name:  "full_combo",
			input: "/delay/0/header/X-Test:val/status/201/get",
			modifiers: []pipelineStep{
				{name: "delay", args: []string{"0"}},
				{name: "header", args: []string{"X-Test:val"}},
				{name: "status", args: []string{"201"}},
			},
			terminal: pipelineStep{name: "get", args: nil},
		},

		// Error cases
		{name: "modifier_no_value", input: "/delay/", wantErr: true},
		{name: "no_terminal", input: "/delay/1", wantErr: true},
		{name: "empty_path", input: "/", wantErr: true},
		{name: "unknown_segment", input: "/unknown/foo", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := parsePipeline(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result: %+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check modifiers
			if len(result.modifiers) != len(tt.modifiers) {
				t.Fatalf("modifier count: want %d, got %d", len(tt.modifiers), len(result.modifiers))
			}
			for i, want := range tt.modifiers {
				got := result.modifiers[i]
				if got.name != want.name {
					t.Errorf("modifier[%d].name: want %q, got %q", i, want.name, got.name)
				}
				if len(got.args) != len(want.args) {
					t.Errorf("modifier[%d].args: want %v, got %v", i, want.args, got.args)
				}
			}

			// Check terminal
			if result.terminal.name != tt.terminal.name {
				t.Errorf("terminal.name: want %q, got %q", tt.terminal.name, result.terminal.name)
			}
			if len(result.terminal.args) != len(tt.terminal.args) {
				t.Errorf("terminal.args: want %v, got %v", tt.terminal.args, result.terminal.args)
			} else {
				for i, want := range tt.terminal.args {
					if result.terminal.args[i] != want {
						t.Errorf("terminal.args[%d]: want %q, got %q", i, want, result.terminal.args[i])
					}
				}
			}
		})
	}
}

func TestPipelineE2E(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t, WithMaxDuration(10*time.Second))

	// B.1 — Simple terminals via pipeline
	simpleTests := []struct {
		name        string
		url         string
		method      string
		status      int
		contentType string
		bodyContain string
	}{
		{"delay+status_418", "/delay/0/status/418", "GET", 418, "", ""},
		{"delay+status_200", "/delay/0/status/200", "GET", 200, "", ""},
		{"delay+status_301", "/delay/0/status/301", "GET", 301, "", ""},
		{"delay+status_500", "/delay/0/status/500", "GET", 500, "", ""},
		{"delay+get", "/delay/0/get", "GET", 200, jsonContentType, `"url"`},
		{"delay+post", "/delay/0/post", "POST", 200, jsonContentType, ""},
		{"delay+put", "/delay/0/put", "PUT", 200, jsonContentType, ""},
		{"delay+delete", "/delay/0/delete", "DELETE", 200, jsonContentType, ""},
		{"delay+patch", "/delay/0/patch", "PATCH", 200, jsonContentType, ""},
		{"delay+html", "/delay/0/html", "GET", 200, htmlContentType, ""},
		{"delay+json", "/delay/0/json", "GET", 200, jsonContentType, ""},
		{"delay+xml", "/delay/0/xml", "GET", 200, "application/xml", ""},
		{"delay+deny", "/delay/0/deny", "GET", 200, "", "YOU SHOULDN'T BE HERE"},
		{"delay+ip", "/delay/0/ip", "GET", 200, "", `"origin"`},
		{"delay+headers", "/delay/0/headers", "GET", 200, "", `"headers"`},
		{"delay+user_agent", "/delay/0/user-agent", "GET", 200, "", `"user-agent"`},
		{"delay+uuid", "/delay/0/uuid", "GET", 200, "", `"uuid"`},
		{"delay+hostname", "/delay/0/hostname", "GET", 200, "", ""},
		{"delay+cookies", "/delay/0/cookies", "GET", 200, "", `"cookies"`},
		{"delay+bytes_1024", "/delay/0/bytes/1024", "GET", 200, "", ""},
		{"delay+etag", "/delay/0/etag/test-etag", "GET", 200, "", ""},
		{"delay+links_5", "/delay/0/links/5", "GET", 200, htmlContentType, ""},
		{"delay+base64", "/delay/0/base64/SFRUUEJJTg==", "GET", 200, "", "HTTPBIN"},
		{"delay+pdf", "/delay/0/pdf", "GET", 200, "application/pdf", ""},
		{"delay+env", "/delay/0/env", "GET", 200, jsonContentType, ""},
		{"delay+anything", "/delay/0/anything", "GET", 200, jsonContentType, `"method"`},
	}

	for _, tt := range simpleTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := newTestRequest(t, tt.method, app.URL(tt.url), nil)
			resp := mustDoRequest(t, app, req)
			assert.StatusCode(t, resp, tt.status)
			if tt.contentType != "" {
				assert.ContentType(t, resp, tt.contentType)
			}
			if tt.bodyContain != "" {
				assert.BodyContains(t, resp, tt.bodyContain)
			}
		})
	}

	// B.2 — Multi-segment terminals (longest-prefix match)
	t.Run("cookies_set", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/cookies/set?test=value"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
	})
	t.Run("cookies_delete", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/cookies/delete?test"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
	})
	t.Run("encoding_utf8", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/encoding/utf8"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})
	t.Run("forms_post", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/forms/post"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, htmlContentType)
	})
	t.Run("dump_request", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/dump/request"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.BodyContains(t, resp, "GET")
	})
	t.Run("basic_auth_401", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/basic-auth/user/pass"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusUnauthorized)
	})
	t.Run("hidden_basic_auth_404", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/hidden-basic-auth/user/pass"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusNotFound)
	})

	// B.3 — Image vanity URLs
	imageTests := []struct {
		name        string
		url         string
		contentType string
	}{
		{"photo_png", "/image/photo.png", "image/png"},
		{"photo_jpeg", "/image/photo.jpeg", "image/jpeg"},
		{"photo_jpg", "/image/photo.jpg", "image/jpeg"},
		{"photo_svg", "/image/photo.svg", "image/svg+xml"},
		{"photo_webp", "/image/photo.webp", "image/webp"},
		{"photo_avif", "/image/photo.avif", "image/avif"},
		{"size_small_png", "/image/size/small/photo.png", "image/png"},
		{"size_medium_png", "/image/size/medium/photo.png", "image/png"},
		{"size_small_jpeg", "/image/size/small/photo.jpeg", "image/jpeg"},
	}
	for _, tt := range imageTests {
		t.Run("image/"+tt.name, func(t *testing.T) {
			t.Parallel()
			req := newTestRequest(t, "GET", app.URL(tt.url), nil)
			resp := mustDoRequest(t, app, req)
			assert.StatusCode(t, resp, http.StatusOK)
			assert.ContentType(t, resp, tt.contentType)
		})
	}

	// Image with modifiers
	t.Run("image/modifier+size", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/response_delay/0/image/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})
	t.Run("image/delay+extension", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/image/photo.jpeg"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/jpeg")
	})

	// B.4 — Redirect chaining
	redirectTests := []struct {
		name     string
		url      string
		location string
	}{
		{"redirect_3_status", "/redirect/3/status/418", "/redirect/2/status/418"},
		{"redirect_2_status", "/redirect/2/status/418", "/redirect/1/status/418"},
		{"redirect_1_status", "/redirect/1/status/418", "/status/418"},
		{"redirect_3_get", "/redirect/3/get", "/redirect/2/get"},
		{"redirect_1_get", "/redirect/1/get", "/get"},
		{"redirect_3_image", "/redirect/3/image/photo.png", "/redirect/2/image/photo.png"},
		{"redirect_1_image", "/redirect/1/image/photo.png", "/image/photo.png"},
		{"redirect_2_bytes", "/redirect/2/bytes/1024", "/redirect/1/bytes/1024"},
	}
	for _, tt := range redirectTests {
		t.Run("redirect/"+tt.name, func(t *testing.T) {
			t.Parallel()
			req := newTestRequest(t, "GET", app.URL(tt.url), nil)
			resp := mustDoRequest(t, app, req)
			assert.StatusCode(t, resp, http.StatusFound)
			assert.Header(t, resp, "Location", tt.location)
		})
	}

	// Redirect with modifiers preserved
	t.Run("redirect/modifier_preserved_3", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/redirect/3/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/delay/0/redirect/2/status/200")
	})
	t.Run("redirect/modifier_preserved_1", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/redirect/1/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/delay/0/get")
	})

	// Absolute redirect
	t.Run("redirect/absolute_2", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/absolute-redirect/2/status/200"), nil)
		req.Host = "host"
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "http://") {
			t.Fatalf("expected absolute URL, got %q", loc)
		}
	})

	// Relative redirect
	t.Run("redirect/relative_2", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/relative-redirect/2/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/relative-redirect/1/status/200")
	})

	// B.5 — Combined modifiers
	t.Run("combined/double_modifier", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/response_delay/0/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})
	t.Run("combined/reverse_order", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/response_delay/0/delay/0/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})

	// B.6 — Query params preserved
	t.Run("query_params_get", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/get?foo=bar"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.BodyContains(t, resp, "foo")
	})
	t.Run("query_params_status", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/response_delay/0/status/200?custom=val"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})
}

func TestPipelineSecurity(t *testing.T) {
	t.Parallel()

	// C.1 — Delay budget / DoS prevention
	t.Run("delay_budget", func(t *testing.T) {
		t.Parallel()

		budgetTests := []struct {
			name   string
			url    string
			status int
		}{
			{"delay_exceeds", "/delay/999/status/200", http.StatusBadRequest},
			{"response_delay_exceeds", "/response_delay/999/get", http.StatusBadRequest},
			{"cumulative_exceeds", "/delay/0.6/response_delay/0.6/get", http.StatusBadRequest},
			{"exact_max_ok", "/delay/1/get", http.StatusOK},
			{"exact_max_combined_ok", "/delay/1/response_delay/0/get", http.StatusOK},
			{"half_plus_half_ok", "/delay/0.5/response_delay/0.5/get", http.StatusOK},
			{"half_plus_over_fail", "/delay/0.5/response_delay/0.6/get", http.StatusBadRequest},
		}
		for _, tt := range budgetTests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					app := setupSynctestApp(t) // MaxDuration = 1s
					req := newTestRequest(t, "GET", app.URL(tt.url), nil)
					resp := mustDoRequest(t, app, req)
					assert.StatusCode(t, resp, tt.status)
				})
			})
		}
	})

	// C.2 — Context cancellation
	t.Run("context_cancel", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			app := setupSynctestApp(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()

			req := newTestRequest(t, "GET", app.URL("/delay/1/get"), nil).WithContext(ctx)
			_, err := app.Client.Do(req)
			if err == nil {
				t.Fatal("expected error from context cancellation")
			}
		})
	})

	// C.3 — Input validation
	t.Run("input_validation", func(t *testing.T) {
		t.Parallel()
		app := setupTestApp(t)

		validationTests := []struct {
			name   string
			url    string
			method string
			status int
		}{
			{"status_0", "/delay/0/status/0", "GET", http.StatusBadRequest},
			{"status_99", "/delay/0/status/99", "GET", http.StatusBadRequest},
			{"status_600", "/delay/0/status/600", "GET", http.StatusBadRequest},
			{"status_negative", "/delay/0/status/-1", "GET", http.StatusBadRequest},
			{"status_alpha", "/delay/0/status/abc", "GET", http.StatusBadRequest},
			{"delay_negative", "/delay/-1/get", "GET", http.StatusBadRequest},
			{"delay_alpha", "/delay/abc/get", "GET", http.StatusBadRequest},
			{"bytes_alpha", "/delay/0/bytes/abc", "GET", http.StatusBadRequest},
			{"redirect_0", "/delay/0/redirect/0/get", "GET", http.StatusBadRequest},
			{"redirect_negative", "/delay/0/redirect/-1/get", "GET", http.StatusBadRequest},
			{"redirect_alpha", "/delay/0/redirect/abc/get", "GET", http.StatusBadRequest},
			{"redirect_over_20", "/redirect/21/get", "GET", http.StatusBadRequest},
			{"redirect_20_ok", "/redirect/20/get", "GET", http.StatusFound},
			{"image_size_huge", "/image/size/huge/x.png", "GET", http.StatusBadRequest},
			{"image_size_svg", "/image/size/large/x.svg", "GET", http.StatusBadRequest},
			{"image_bad_ext", "/image/size/large/x.bmp", "GET", http.StatusBadRequest},
			{"image_bad_param", "/image/badparam/val/x.png", "GET", http.StatusBadRequest},
		}
		for _, tt := range validationTests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				req := newTestRequest(t, tt.method, app.URL(tt.url), nil)
				resp := mustDoRequest(t, app, req)
				assert.StatusCode(t, resp, tt.status)
			})
		}
	})

	// C.4 — Method restrictions
	t.Run("method_restrictions", func(t *testing.T) {
		t.Parallel()
		app := setupTestApp(t)

		methodTests := []struct {
			name   string
			url    string
			method string
			status int
		}{
			{"post_on_get", "/delay/0/get", "POST", http.StatusMethodNotAllowed},
			{"get_on_post", "/delay/0/post", "GET", http.StatusMethodNotAllowed},
			{"delete_on_put", "/delay/0/put", "DELETE", http.StatusMethodNotAllowed},
			{"get_encoding_ok", "/delay/0/encoding/utf8", "GET", http.StatusOK},
			{"post_encoding_fail", "/delay/0/encoding/utf8", "POST", http.StatusMethodNotAllowed},
			{"get_status_ok", "/delay/0/status/200", "GET", http.StatusOK},
			{"post_status_ok", "/delay/0/status/200", "POST", http.StatusOK},
			{"delete_anything_ok", "/delay/0/anything", "DELETE", http.StatusOK},
		}
		for _, tt := range methodTests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				req := newTestRequest(t, tt.method, app.URL(tt.url), nil)
				resp := mustDoRequest(t, app, req)
				assert.StatusCode(t, resp, tt.status)
			})
		}
	})

	// C.5 — Segment limits
	t.Run("too_many_segments", func(t *testing.T) {
		t.Parallel()
		app := setupTestApp(t)
		path := "/delay/0" + strings.Repeat("/delay/0", 10) + "/get"
		req := newTestRequest(t, "GET", app.URL(path), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})

	// C.7 — HEAD method through pipeline (autohead middleware)
	t.Run("head_status", func(t *testing.T) {
		t.Parallel()
		app := setupTestApp(t)
		req := newTestRequest(t, "HEAD", app.URL("/delay/0/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})
}

func TestPipelineDelay(t *testing.T) {
	t.Parallel()

	t.Run("single_delay_500ms", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			app := setupSynctestApp(t)
			start := time.Now()
			req := newTestRequest(t, "GET", app.URL("/delay/500ms/status/200"), nil)
			resp := mustDoRequest(t, app, req)
			elapsed := time.Since(start)
			assert.StatusCode(t, resp, http.StatusOK)
			if elapsed < 500*time.Millisecond {
				t.Fatalf("expected >= 500ms delay, got %s", elapsed)
			}
			timings := decodeServerTimings(resp.Header.Get("Server-Timing"))
			if _, ok := timings["pipeline_delay"]; !ok {
				t.Fatal("expected pipeline_delay in Server-Timing")
			}
		})
	})

	t.Run("double_delay_cumulative", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			app := setupSynctestApp(t)
			start := time.Now()
			req := newTestRequest(t, "GET", app.URL("/delay/500ms/response_delay/500ms/get"), nil)
			resp := mustDoRequest(t, app, req)
			elapsed := time.Since(start)
			assert.StatusCode(t, resp, http.StatusOK)
			if elapsed < 1*time.Second {
				t.Fatalf("expected >= 1s cumulative delay, got %s", elapsed)
			}
		})
	})

	t.Run("zero_delay", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			app := setupSynctestApp(t)
			req := newTestRequest(t, "GET", app.URL("/delay/0/get"), nil)
			resp := mustDoRequest(t, app, req)
			assert.StatusCode(t, resp, http.StatusOK)
		})
	})
}

func TestPipelineBackwardCompat(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	// Verify that existing routes are NOT affected by subtree registrations
	compatTests := []struct {
		name   string
		url    string
		method string
		status int
	}{
		{"get", "/get", "GET", http.StatusOK},
		{"post", "/post", "POST", http.StatusOK},
		{"status_418", "/status/418", "GET", 418},
		{"image_accept", "/image", "GET", http.StatusOK},
		{"image_png", "/image/png", "GET", http.StatusOK},
		{"image_jpeg", "/image/jpeg", "GET", http.StatusOK},
		{"cookies", "/cookies", "GET", http.StatusOK},
		{"anything", "/anything", "GET", http.StatusOK},
		{"index", "/", "GET", http.StatusOK},
	}
	for _, tt := range compatTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := newTestRequest(t, tt.method, app.URL(tt.url), nil)
			resp := mustDoRequest(t, app, req)
			assert.StatusCode(t, resp, tt.status)
		})
	}

	// Redirect backward compat (must use synctest for delay tests)
	t.Run("redirect_1", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/redirect/1"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/get")
	})
	t.Run("redirect_3", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/redirect/3"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/relative-redirect/2")
	})
	t.Run("absolute_redirect_1", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/absolute-redirect/1"), nil)
		req.Host = "host"
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "http://") {
			t.Fatalf("expected absolute URL, got %q", loc)
		}
	})
	t.Run("relative_redirect_1", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/relative-redirect/1"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/get")
	})

	// Delay backward compat
	t.Run("delay_0", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			app := setupSynctestApp(t)
			req := newTestRequest(t, "GET", app.URL("/delay/0"), nil)
			resp := mustDoRequest(t, app, req)
			assert.StatusCode(t, resp, http.StatusOK)
		})
	})

	// Mix backward compat
	// Note: /mix sets status codes without content type, so we check status directly
	t.Run("mix_status", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/mix/s=503"), nil)
		resp := mustDoRequest(t, app, req)
		assert.Equal(t, resp.StatusCode, 503, "wrong status code")
	})
	t.Run("mix_delay_status", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/mix/d=0/s=418"), nil)
		resp := mustDoRequest(t, app, req)
		assert.Equal(t, resp.StatusCode, 418, "wrong status code")
	})
}

func TestPipelineWithPrefix(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t, WithPrefix("/api"), WithMaxDuration(10*time.Second))

	t.Run("status_418", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/api/delay/0/status/418"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 418)
	})
	t.Run("image_vanity", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/api/image/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})
	t.Run("redirect_with_prefix", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/api/redirect/2/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/api/redirect/1/get")
	})
	t.Run("redirect_modifier_prefix", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/api/delay/0/redirect/1/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/api/delay/0/get")
	})
}

func TestPipelineGetURLRewrite(t *testing.T) {
	t.Parallel()

	// Verify that getURL returns the canonical path after pipeline rewrite
	app := setupTestApp(t)

	t.Run("get_url_contains_canonical", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		body := must.ReadAll(t, resp.Body)
		// The URL in the response should contain /get, not /delay/0/get
		assert.Contains(t, body, "/get", "response url field")
	})
}

func BenchmarkParsePipeline(b *testing.B) {
	paths := []string{
		"/delay/1/status/418",
		"/delay/1/response_delay/2/get",
		"/image/size/large/photo.png",
		"/delay/1/redirect/3/image/photo.png",
	}
	for _, p := range paths {
		b.Run(p, func(b *testing.B) {
			for b.Loop() {
				parsePipeline(p)
			}
		})
	}
}

func TestBuildModifierPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modifiers []pipelineStep
		want      string
	}{
		{"empty", nil, ""},
		{"single", []pipelineStep{{name: "delay", args: []string{"1"}}}, "/delay/1"},
		{"double", []pipelineStep{
			{name: "delay", args: []string{"1"}},
			{name: "response_delay", args: []string{"2"}},
		}, "/delay/1/response_delay/2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildModifierPrefix(tt.modifiers)
			assert.Equal(t, got, tt.want, "buildModifierPrefix")
		})
	}
}

func TestPipelineRedirectNoDestination(t *testing.T) {
	t.Parallel()

	// redirect/1 without destination should redirect to /get
	t.Run("redirect_1_no_dest", func(t *testing.T) {
		t.Parallel()

		app := setupTestApp(t)
		// This goes to existing route /redirect/{numRedirects}, not pipeline
		req := newTestRequest(t, "GET", app.URL("/redirect/1"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/get")
	})

	// redirect/2/get (pipeline) with count decrement
	t.Run("redirect_chain_to_get", func(t *testing.T) {
		t.Parallel()

		app := setupTestApp(t, WithMaxDuration(10*time.Second))
		req := newTestRequest(t, "GET", app.URL("/redirect/2/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
		assert.Header(t, resp, "Location", "/redirect/1/get")

		// Follow to /redirect/1/get
		req2 := newTestRequest(t, "GET", app.URL("/redirect/1/get"), nil)
		resp2 := mustDoRequest(t, app, req2)
		assert.StatusCode(t, resp2, http.StatusFound)
		assert.Header(t, resp2, "Location", "/get")
	})
}

func TestPipelineImageNoArgs(t *testing.T) {
	t.Parallel()

	// /image/ (empty) should use Accept header
	app := setupTestApp(t)
	req := newTestRequest(t, "GET", app.URL("/image/"), nil)
	req.Header.Set("Accept", "image/png")
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusOK)
	assert.ContentType(t, resp, "image/png")
}

func TestPipelineTerminalTable(t *testing.T) {
	t.Parallel()

	// Verify all expected terminals are in the lookup table
	expectedTerminals := []string{
		"get", "post", "put", "delete", "patch", "head",
		"status", "bytes", "stream", "stream-bytes", "etag", "range",
		"links", "cache", "base64",
		"basic-auth", "hidden-basic-auth", "digest-auth",
		"gzip", "deflate", "html", "json", "xml", "robots.txt", "deny",
		"ip", "headers", "user-agent", "uuid", "hostname", "bearer",
		"cookies", "cookies/set", "cookies/delete",
		"encoding/utf8", "forms/post", "dump/request",
		"drip", "sse", "unstable", "anything", "env", "pdf", "trailers",
		"image", "redirect", "absolute-redirect", "relative-redirect",
	}

	for _, name := range expectedTerminals {
		if _, ok := pipelineTerminals[name]; !ok {
			t.Errorf("expected terminal %q not found in pipelineTerminals", name)
		}
	}
}

func TestPipelineRedirectCountLimits(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t, WithMaxDuration(10*time.Second))

	t.Run("count_0_rejected", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/redirect/0/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})
	t.Run("count_21_rejected", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/redirect/21/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})
	t.Run("count_20_ok", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/redirect/20/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusFound)
	})
}

func TestPipelineResponseDelayModifier(t *testing.T) {
	t.Parallel()

	// Test response_delay as first segment (triggers subtree route)
	t.Run("response_delay_status", func(t *testing.T) {
		t.Parallel()
		app := setupTestApp(t)
		req := newTestRequest(t, "GET", app.URL("/response_delay/0/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})

	t.Run("response_delay_json", func(t *testing.T) {
		t.Parallel()
		app := setupTestApp(t)
		req := newTestRequest(t, "GET", app.URL("/response_delay/0/json"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, jsonContentType)
	})
}

func TestPipelineDelayWithRange(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		app := setupSynctestApp(t)
		req := newTestRequest(t, "GET", app.URL("/delay/0-1/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})
}

func TestPipelineAbsoluteRedirectChain(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t, WithMaxDuration(10*time.Second))

	req := newTestRequest(t, "GET", app.URL("/absolute-redirect/2/status/200"), nil)
	req.Host = "testhost"
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusFound)
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "http://") {
		t.Fatalf("expected absolute URL, got %q", loc)
	}
	if !strings.Contains(loc, "absolute-redirect/1/status/200") {
		t.Fatalf("expected absolute-redirect/1/status/200 in %q", loc)
	}
}

func TestPipelineUnknownImageParam(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	// /image/badparam should fail
	req := newTestRequest(t, "GET", app.URL("/image/badparam"), nil)
	resp := mustDoRequest(t, app, req)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected error for unknown image parameter, got %d", resp.StatusCode)
	}
}

func TestPipelinePathRewriteForGet(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	// The "url" field in the JSON response should contain the canonical /get path
	req := newTestRequest(t, "GET", app.URL("/delay/0/get"), nil)
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusOK)

	result := must.Unmarshal[map[string]any](t, resp.Body)
	urlStr, ok := result["url"].(string)
	if !ok {
		t.Fatalf("expected url field in response, got %v", result)
	}
	if !strings.HasSuffix(urlStr, "/get") {
		t.Fatalf("expected URL to end with /get, got %q", urlStr)
	}
}

func TestPipelineDigestAuth(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req := newTestRequest(t, "GET", app.URL("/delay/0/digest-auth/auth/user/pass"), nil)
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusUnauthorized)
}

func TestPipelineCache(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	t.Run("cache_no_args", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/cache"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})
	t.Run("cache_with_seconds", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/cache/60"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.Header(t, resp, "Cache-Control", fmt.Sprintf("public, max-age=%d", 60))
	})
}

func TestPipelineStream(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	t.Run("stream_5", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/stream/5"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		body := must.ReadAll(t, resp.Body)
		// Each line is a JSON object, should have 5 lines
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if len(lines) != 5 {
			t.Fatalf("expected 5 lines, got %d", len(lines))
		}
	})
}

// Tests for pipeline expansion: status/header modifiers, body terminal
func TestPipelineExpansionE2E(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t, WithMaxDuration(10*time.Second))

	t.Run("body_status_422", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/status/422/body/SGVsbG8="), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 422)
		assert.BodyContains(t, resp, "Hello")
	})

	t.Run("delay_status_get", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/status/422/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 422)
		assert.ContentType(t, resp, jsonContentType)
	})

	t.Run("header_status_200", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/header/X-Custom:test/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 200)
		assert.Header(t, resp, "X-Custom", "test")
	})

	t.Run("full_combo", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/header/X-Test:val/status/201/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 201)
		assert.Header(t, resp, "X-Test", "val")
		assert.ContentType(t, resp, jsonContentType)
	})

	t.Run("body_url_safe_b64", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/status/200/body/SGVsbG8gV29ybGQ"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 200)
		assert.BodyContains(t, resp, "Hello World")
	})

	t.Run("header_disallowed", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/header/Content-Type:text%2Fhtml/status/200"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})

	// Backward compat: /status/418 still works as terminal
	t.Run("backward_compat_status_418", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/status/418"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 418)
	})

	// Backward compat: /delay/0/status/418 still works
	t.Run("backward_compat_delay_status", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/status/418"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 418)
	})

	// Body terminal as first segment
	t.Run("body_alone", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/body/SGVsbG8="), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 200)
		assert.BodyContains(t, resp, "Hello")
	})

	// Status modifier with existing delay modifier
	t.Run("status_422_body_hello", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/status/422/body/SGVsbG8="), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, 422)
		assert.BodyContains(t, resp, "Hello")
	})
}

// Tests for image pipeline gradient tokens
func TestPipelineImageGradientE2E(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t, WithMaxDuration(10*time.Second))

	t.Run("gradient_warm_png", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/gradient/warm/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})

	t.Run("gradient_with_size", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/gradient/cool/size/medium/photo.jpeg"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/jpeg")
	})

	t.Run("gradient_with_delay", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/image/gradient/warm/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})

	t.Run("nocache_gradient", func(t *testing.T) {
		t.Parallel()
		req1 := newTestRequest(t, "GET", app.URL("/image/no-cache/gradient/warm/size/small/photo.png"), nil)
		resp1 := mustDoRequest(t, app, req1)
		assert.StatusCode(t, resp1, http.StatusOK)
		body1, _ := io.ReadAll(resp1.Body)

		req2 := newTestRequest(t, "GET", app.URL("/image/no-cache/gradient/warm/size/small/photo.png"), nil)
		resp2 := mustDoRequest(t, app, req2)
		assert.StatusCode(t, resp2, http.StatusOK)
		body2, _ := io.ReadAll(resp2.Body)

		if bytes.Equal(body1, body2) {
			t.Fatal("no-cache requests should produce different images")
		}
	})

	t.Run("invalid_preset_pipeline", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/gradient/invalid/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})

	// Backward compat
	t.Run("backward_compat_image_png", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})

	t.Run("backward_compat_image_photo_png", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})

	t.Run("backward_compat_image_size_small", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})
}

// Tests for image cache behavior
func TestImageCacheDeterministic(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req1 := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png"), nil)
	resp1 := mustDoRequest(t, app, req1)
	assert.StatusCode(t, resp1, http.StatusOK)
	body1, _ := io.ReadAll(resp1.Body)
	etag1 := resp1.Header.Get("ETag")

	req2 := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png"), nil)
	resp2 := mustDoRequest(t, app, req2)
	assert.StatusCode(t, resp2, http.StatusOK)
	body2, _ := io.ReadAll(resp2.Body)
	etag2 := resp2.Header.Get("ETag")

	if !bytes.Equal(body1, body2) {
		t.Fatal("deterministic requests should produce identical images")
	}
	assert.Equal(t, etag1, etag2, "ETags should match for identical requests")
}

func TestImageNocache(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req1 := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png?nocache=1"), nil)
	resp1 := mustDoRequest(t, app, req1)
	assert.StatusCode(t, resp1, http.StatusOK)
	body1, _ := io.ReadAll(resp1.Body)
	cc1 := resp1.Header.Get("Cache-Control")
	assert.Equal(t, cc1, "no-cache, no-store, must-revalidate", "wrong cache-control for nocache")

	req2 := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png?nocache=1"), nil)
	resp2 := mustDoRequest(t, app, req2)
	body2, _ := io.ReadAll(resp2.Body)

	if bytes.Equal(body1, body2) {
		t.Fatal("nocache requests should produce different images")
	}
}

func TestImageNocachePath(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req1 := newTestRequest(t, "GET", app.URL("/image/no-cache/size/small/photo.png"), nil)
	resp1 := mustDoRequest(t, app, req1)
	assert.StatusCode(t, resp1, http.StatusOK)
	body1, _ := io.ReadAll(resp1.Body)

	req2 := newTestRequest(t, "GET", app.URL("/image/no-cache/size/small/photo.png"), nil)
	resp2 := mustDoRequest(t, app, req2)
	body2, _ := io.ReadAll(resp2.Body)

	if bytes.Equal(body1, body2) {
		t.Fatal("no-cache path requests should produce different images")
	}
}

func TestImageGradientQueryParams(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	t.Run("preset", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/png?gradient=warm&size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.ContentType(t, resp, "image/png")
	})

	t.Run("custom_colors", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/png?color1=FF0000&color2=0000FF&size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})

	t.Run("noise", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/png?gradient=warm&noise=32&size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
	})

	t.Run("error_invalid_preset", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/png?gradient=nonexistent&size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})

	t.Run("error_preset_with_colors", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/png?gradient=warm&color1=FF0000&size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
	})

	t.Run("changes_output", func(t *testing.T) {
		t.Parallel()
		req1 := newTestRequest(t, "GET", app.URL("/image/png?gradient=warm&size=small"), nil)
		resp1 := mustDoRequest(t, app, req1)
		body1, _ := io.ReadAll(resp1.Body)

		req2 := newTestRequest(t, "GET", app.URL("/image/png?gradient=cool&size=small"), nil)
		resp2 := mustDoRequest(t, app, req2)
		body2, _ := io.ReadAll(resp2.Body)

		if bytes.Equal(body1, body2) {
			t.Fatal("different gradients should produce different images")
		}
	})
}

func TestImageAvifStatic(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req := newTestRequest(t, "GET", app.URL("/image/avif"), nil)
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusOK)
	assert.ContentType(t, resp, "image/avif")

	body, _ := io.ReadAll(resp.Body)
	if len(body) <= 36 {
		t.Fatalf("expected AVIF > 36 bytes (real image), got %d bytes", len(body))
	}
}

func TestImageConvertNoTool(t *testing.T) {
	// Not parallel: mutates the package-level execLookPath variable.
	origLookPath := execLookPath
	execLookPath = func(string) (string, error) {
		return "", errors.New("not found")
	}
	app := setupTestApp(t)
	execLookPath = origLookPath

	t.Run("avif_501", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/avif?size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusNotImplemented)
		body := must.ReadAll(t, resp.Body)
		assert.Contains(t, body, "no avif conversion tool", "body")
	})

	t.Run("webp_501", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/webp?size=small"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusNotImplemented)
		body := must.ReadAll(t, resp.Body)
		assert.Contains(t, body, "no webp conversion tool", "body")
	})
}

func TestPipelineBodyErrors(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	t.Run("invalid_base64_body", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/delay/0/body/!!!invalid!!!"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusBadRequest)
		body := must.ReadAll(t, resp.Body)
		assert.Contains(t, body, "invalid base64", "body")
	})

	t.Run("invalid_base64_status_body", func(t *testing.T) {
		t.Parallel()
		// status/200 modifier overrides the 400 from the body terminal,
		// but the response body still contains the base64 error.
		req := newTestRequest(t, "GET", app.URL("/status/200/body/!!!bad"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		body := must.ReadAll(t, resp.Body)
		assert.Contains(t, body, "invalid base64", "body")
	})
}

func TestImageAcceptAvif(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req := newTestRequest(t, "GET", app.URL("/image"), nil)
	req.Header.Set("Accept", "image/avif")
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusOK)
	assert.ContentType(t, resp, "image/avif")
}

func TestImageCacheControlValue(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	t.Run("default_cache_control", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.Equal(t, resp.Header.Get("Cache-Control"), "public, max-age=86400", "Cache-Control header")
	})

	t.Run("nocache_cache_control", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "GET", app.URL("/image/size/small/photo.png?nocache=1"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusOK)
		assert.Equal(t, resp.Header.Get("Cache-Control"), "no-cache, no-store, must-revalidate", "Cache-Control header")
	})
}

func TestPipelineMethodRestrictionWithStatusOverride(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	t.Run("post_status_422_get", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "POST", app.URL("/status/422/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusMethodNotAllowed)
	})

	t.Run("put_delay_status_get", func(t *testing.T) {
		t.Parallel()
		req := newTestRequest(t, "PUT", app.URL("/delay/0/status/201/get"), nil)
		resp := mustDoRequest(t, app, req)
		assert.StatusCode(t, resp, http.StatusMethodNotAllowed)
	})
}

func TestPipelineMultipleHeaders(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req := newTestRequest(t, "GET", app.URL("/header/X-A:1/header/X-B:2/status/200"), nil)
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusOK)
	assert.Header(t, resp, "X-A", "1")
	assert.Header(t, resp, "X-B", "2")
}

func TestImageAcceptWithGradient(t *testing.T) {
	t.Parallel()
	app := setupTestApp(t)

	req := newTestRequest(t, "GET", app.URL("/image?gradient=warm&size=small"), nil)
	req.Header.Set("Accept", "image/png")
	resp := mustDoRequest(t, app, req)
	assert.StatusCode(t, resp, http.StatusOK)
	assert.ContentType(t, resp, "image/png")
}

// Benchmark for image generation
func BenchmarkGenerateImage(b *testing.B) {
	grad := defaultGradient()
	sizes := []struct {
		name string
		size int
	}{
		{"png_small", 50 * 1024},
		{"png_medium", 500 * 1024},
		{"jpeg_small", 50 * 1024},
	}
	for _, s := range sizes {
		format := "png"
		if strings.HasPrefix(s.name, "jpeg") {
			format = "jpeg"
		}
		b.Run(s.name, func(b *testing.B) {
			for b.Loop() {
				generateImage(format, s.size, grad, 42)
			}
		})
	}
}

func BenchmarkCachedGenerateImage(b *testing.B) {
	cache := newImageCache(256)
	grad := defaultGradient()
	key := imageCacheKey{format: "png", targetSize: 50 * 1024, grad: grad}

	// Pre-populate cache
	data, ct, _ := generateImage("png", 50*1024, grad, 42)
	cache.put(key, imageCacheEntry{data: data, contentType: ct, etag: `"test"`})

	b.Run("cached_hit", func(b *testing.B) {
		for b.Loop() {
			cache.get(key)
		}
	})
}
