package httpbin

import (
	"bytes"
	"net"
	"net/http"
	"time"
)

// Default configuration values
const (
	DefaultMaxBodySize int64 = 1024 * 1024
	DefaultMaxDuration       = 10 * time.Second
	DefaultHostname          = "go-httpbin"
)

// DefaultParams defines default parameter values
type DefaultParams struct {
	// for the /drip endpoint
	DripDuration time.Duration
	DripDelay    time.Duration
	DripNumBytes int64

	// for the /sse endpoint
	SSECount    int
	SSEDuration time.Duration
	SSEDelay    time.Duration
}

// DefaultDefaultParams defines the DefaultParams that are used by default. In
// general, these should match the original httpbin.org's defaults.
var DefaultDefaultParams = DefaultParams{
	DripDuration: 2 * time.Second,
	DripDelay:    2 * time.Second,
	DripNumBytes: 10,
	SSECount:     10,
	SSEDuration:  5 * time.Second,
	SSEDelay:     0,
}

type headersProcessorFunc func(h http.Header) http.Header

// HTTPBin contains the business logic
type HTTPBin struct {
	// Max size of an incoming request or generated response body, in bytes
	MaxBodySize int64

	// Max duration of a request, for those requests that allow user control
	// over timing (e.g. /delay)
	MaxDuration time.Duration

	// Observer called with the result of each handled request
	Observer Observer

	// Default parameter values
	DefaultParams DefaultParams

	// Set of hosts to which the /redirect-to endpoint will allow redirects
	AllowedRedirectDomains map[string]struct{}

	// If true, endpoints that allow clients to specify a response
	// Conntent-Type will NOT escape HTML entities in the response body, which
	// can enable (e.g.) reflected XSS attacks.
	//
	// This configuration is only supported for backwards compatibility if
	// absolutely necessary.
	unsafeAllowDangerousResponses bool

	// The operator-controlled environment variables filtered from
	// the process environment, based on named HTTPBIN_ prefix.
	env map[string]string

	// Pre-computed error message for the /redirect-to endpoint, based on
	// -allowed-redirect-domains/ALLOWED_REDIRECT_DOMAINS
	forbiddenRedirectError string

	// The hostname to expose via /hostname.
	hostname string

	// The app's http handler
	handler http.Handler

	// Optional prefix under which the app will be served
	prefix string

	// Pre-rendered templates
	indexHTML     []byte
	formsPostHTML []byte

	// Pre-computed map of special cases for the /status endpoint
	statusSpecialCases map[int]*statusCase

	// Optional function to control which headers are excluded from the
	// /headers response
	excludeHeadersProcessor headersProcessorFunc

	// Max number of SSE events to send, based on rough estimate of single
	// event's size
	maxSSECount int64

	// Trusted proxy CIDRs for X-Forwarded-For header parsing.
	// nil = trust all proxy headers (backward compatible default).
	// empty = trust no proxy headers, use RemoteAddr only.
	trustedProxies []*net.IPNet

	// Whether trusted proxies have been explicitly configured
	trustedProxiesConfigured bool

	// Version string for the /version endpoint
	version string

	rateLimiter           *ipRateLimiter
	rateLimitUseSubnets   bool
	maxConcurrentRequests int
}

// New creates a new HTTPBin instance
func New(opts ...OptionFunc) *HTTPBin {
	h := &HTTPBin{
		MaxBodySize:   DefaultMaxBodySize,
		MaxDuration:   DefaultMaxDuration,
		DefaultParams: DefaultDefaultParams,
		hostname:      DefaultHostname,
	}
	for _, opt := range opts {
		opt(h)
	}

	// pre-compute some configuration values and pre-render templates
	tmplData := struct{ Prefix string }{Prefix: h.prefix}
	h.indexHTML = mustRenderTemplate("index.html.tmpl", tmplData)
	h.formsPostHTML = mustRenderTemplate("forms-post.html.tmpl", tmplData)
	h.statusSpecialCases = createSpecialCases(h.prefix)

	// compute max Server-Sent Event count based on max request size and rough
	// estimate of a single event's size on the wire
	var buf bytes.Buffer
	writeServerSentEvent(&buf, 999, time.Now(), "")
	h.maxSSECount = h.MaxBodySize / int64(buf.Len())

	h.handler = h.Handler()
	return h
}

// ServeHTTP implememnts the http.Handler interface.
func (h *HTTPBin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// getClientIP returns the client IP for the given request, using the
// configured trusted proxy settings.
func (h *HTTPBin) getClientIP(r *http.Request) string {
	return getClientIP(r, h.trustedProxies, h.trustedProxiesConfigured)
}

// Assert that HTTPBin implements http.Handler interface
var _ http.Handler = &HTTPBin{}

// Handler returns an http.Handler that exposes all HTTPBin endpoints
func (h *HTTPBin) Handler() http.Handler {
	mux := http.NewServeMux()

	// Endpoints restricted to specific methods
	mux.HandleFunc("DELETE /delete", h.RequestWithBody)
	mux.HandleFunc("GET /{$}", h.Index)
	mux.HandleFunc("GET /encoding/utf8", h.UTF8)
	mux.HandleFunc("GET /forms/post", h.FormsPost)
	mux.HandleFunc("GET /get", h.Get)
	mux.HandleFunc("GET /websocket/echo", h.WebSocketEcho)
	mux.HandleFunc("HEAD /head", h.Get)
	mux.HandleFunc("PATCH /patch", h.RequestWithBody)
	mux.HandleFunc("POST /post", h.RequestWithBody)
	mux.HandleFunc("PUT /put", h.RequestWithBody)

	// Endpoints that accept any methods
	mux.HandleFunc("/absolute-redirect/{numRedirects}", h.AbsoluteRedirect)
	mux.HandleFunc("/anything", h.Anything)
	mux.HandleFunc("/anything/", h.Anything)
	mux.HandleFunc("/base64/{data}", h.Base64)
	mux.HandleFunc("/base64/{operation}/{data}", h.Base64)
	mux.HandleFunc("/basic-auth/{user}/{password}", h.BasicAuth)
	mux.HandleFunc("/bearer", h.Bearer)
	mux.HandleFunc("/bytes/{numBytes}", h.Bytes)
	mux.HandleFunc("/cache", h.Cache)
	mux.HandleFunc("/cache/{numSeconds}", h.CacheControl)
	mux.HandleFunc("/cookies", h.Cookies)
	mux.HandleFunc("/cookies/delete", h.DeleteCookies)
	mux.HandleFunc("/cookies/set", h.SetCookies)
	mux.HandleFunc("/deflate", h.Deflate)
	mux.HandleFunc("/delay/{duration}", h.Delay)
	mux.HandleFunc("/deny", h.Deny)
	mux.HandleFunc("/digest-auth/{qop}/{user}/{password}", h.DigestAuth)
	mux.HandleFunc("/digest-auth/{qop}/{user}/{password}/{algorithm}", h.DigestAuth)
	mux.HandleFunc("/drip", h.Drip)
	mux.HandleFunc("/dump/request", h.DumpRequest)
	mux.HandleFunc("/env", h.Env)
	mux.HandleFunc("/etag/{etag}", h.ETag)
	mux.HandleFunc("/gzip", h.Gzip)
	mux.HandleFunc("/headers", h.Headers)
	mux.HandleFunc("/hidden-basic-auth/{user}/{password}", h.HiddenBasicAuth)
	mux.HandleFunc("/hostname", h.Hostname)
	mux.HandleFunc("/html", h.HTML)
	mux.HandleFunc("/image", h.ImageAccept)
	mux.HandleFunc("/image/{kind}", h.Image)
	mux.HandleFunc("/ip", h.IP)
	mux.HandleFunc("/json", h.JSON)
	mux.HandleFunc("/links/{numLinks}", h.Links)
	mux.HandleFunc("/links/{numLinks}/{offset}", h.Links)
	mux.HandleFunc("/range/{numBytes}", h.Range)
	mux.HandleFunc("/redirect-to", h.RedirectTo)
	mux.HandleFunc("/redirect/{numRedirects}", h.Redirect)
	mux.HandleFunc("/relative-redirect/{numRedirects}", h.RelativeRedirect)
	mux.HandleFunc("/response-headers", h.ResponseHeaders)
	mux.HandleFunc("/robots.txt", h.Robots)
	mux.HandleFunc("/sse", h.SSE)
	mux.HandleFunc("/status/{code}", h.Status)
	mux.HandleFunc("/stream-bytes/{numBytes}", h.StreamBytes)
	mux.HandleFunc("/stream/{numLines}", h.Stream)
	mux.HandleFunc("/trailers", h.Trailers)
	mux.HandleFunc("/unstable", h.Unstable)
	mux.HandleFunc("POST /upload", h.RequestWithBodyDiscard)
	mux.HandleFunc("PUT /upload", h.RequestWithBodyDiscard)
	mux.HandleFunc("PATCH /upload", h.RequestWithBodyDiscard)
	mux.HandleFunc("/user-agent", h.UserAgent)
	mux.HandleFunc("/uuid", h.UUID)
	mux.HandleFunc("/xml", h.XML)

	// New endpoints
	mux.HandleFunc("/close", h.Close)
	mux.HandleFunc("/negotiate", h.Negotiate)
	mux.HandleFunc("/pdf", h.PDF)
	mux.HandleFunc("/problem", h.ProblemDetails)
	mux.HandleFunc("/version", h.Version)
	mux.HandleFunc("POST /echo", h.Echo)
	mux.HandleFunc("PUT /echo", h.Echo)
	mux.HandleFunc("PATCH /echo", h.Echo)

	mux.HandleFunc("/mix/", h.Mix)

	// Pipeline composable subtree routes
	mux.HandleFunc("/delay/", h.Pipeline)
	mux.HandleFunc("/response_delay/", h.Pipeline)
	mux.HandleFunc("/image/", h.Pipeline)
	mux.HandleFunc("/redirect/", h.Pipeline)
	mux.HandleFunc("/absolute-redirect/", h.Pipeline)
	mux.HandleFunc("/relative-redirect/", h.Pipeline)

	// existing httpbin endpoints that we do not support
	mux.HandleFunc("/brotli", notImplementedHandler)

	// Apply global middleware
	var handler http.Handler
	handler = mux
	handler = limitRequestSize(h.MaxBodySize, handler)
	handler = responseDelay(h.MaxDuration, handler)
	handler = preflight(handler)
	handler = autohead(handler)

	if h.maxConcurrentRequests > 0 {
		handler = maxConcurrent(h.maxConcurrentRequests, handler)
	}
	if h.rateLimiter != nil {
		handler = rateLimitMiddleware(h.rateLimiter, h.getClientIP, h.rateLimitUseSubnets, handler)
	}

	if h.prefix != "" {
		handler = http.StripPrefix(h.prefix, handler)
	}

	if h.Observer != nil {
		handler = observe(h.Observer, h.getClientIP, handler)
	}

	return handler
}

func (h *HTTPBin) setExcludeHeaders(excludeHeaders string) {
	regex := createFullExcludeRegex(excludeHeaders)
	if regex != nil {
		h.excludeHeadersProcessor = createExcludeHeadersProcessor(regex)
	}
}

// mustEscapeResponse returns true if the response body should be HTML-escaped
// to prevent XSS and similar attacks when rendered by a web browser.
func (h *HTTPBin) mustEscapeResponse(contentType string) bool {
	if h.unsafeAllowDangerousResponses {
		return false
	}
	return isDangerousContentType(contentType)
}
