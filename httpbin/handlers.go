package httpbin

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/galti3r/go-httpbin/v3/httpbin/digest"
	"github.com/galti3r/go-httpbin/v3/httpbin/websocket"
)

var nilValues = url.Values{}

func notImplementedHandler(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, nil)
}

// Index renders an HTML index page
func (h *HTTPBin) Index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, nil)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' camo.githubusercontent.com")
	writeHTML(w, h.indexHTML, http.StatusOK)
}

// Env - returns environment variables with HTTPBIN_ prefix, if any pre-configured by operator
func (h *HTTPBin) Env(w http.ResponseWriter, _ *http.Request) {
	writeJSON(http.StatusOK, w, &envResponse{
		Env: h.env,
	})
}

// FormsPost renders an HTML form that submits a request to the /post endpoint
func (h *HTTPBin) FormsPost(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, h.formsPostHTML, http.StatusOK)
}

// UTF8 renders an HTML encoding stress test
func (h *HTTPBin) UTF8(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, mustStaticAsset("utf8.html"), http.StatusOK)
}

// Get handles HTTP GET requests
func (h *HTTPBin) Get(w http.ResponseWriter, r *http.Request) {
	writeJSON(http.StatusOK, w, &noBodyResponse{
		Args:    r.URL.Query(),
		Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
		Method:  r.Method,
		Origin:  h.getClientIP(r),
		URL:     getURL(r).String(),
	})
}

// Anything returns anything that is passed to request.
func (h *HTTPBin) Anything(w http.ResponseWriter, r *http.Request) {
	// Short-circuit for HEAD requests, which should be handled like regular
	// GET requests (where the autohead middleware will take care of discarding
	// the body)
	if r.Method == http.MethodHead {
		h.Get(w, r)
		return
	}
	// All other requests will be handled the same.  For compatibility with
	// httpbin, the /anything endpoint even allows GET requests to have bodies.
	h.RequestWithBody(w, r)
}

// RequestWithBody handles POST, PUT, and PATCH requests by responding with a
// JSON representation of the incoming request.
func (h *HTTPBin) RequestWithBody(w http.ResponseWriter, r *http.Request) {
	resp := &bodyResponse{
		Args:    r.URL.Query(),
		Files:   nilValues,
		Form:    nilValues,
		Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
		Method:  r.Method,
		Origin:  h.getClientIP(r),
		URL:     getURL(r).String(),
	}

	if err := parseBody(r, resp); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("error parsing request body: %w", err))
		return
	}

	writeJSON(http.StatusOK, w, resp)
}

// RequestWithBodyDiscard handles POST, PUT, and PATCH requests by responding with a
// JSON representation of the incoming request without body data
func (h *HTTPBin) RequestWithBodyDiscard(w http.ResponseWriter, r *http.Request) {
	resp := &discardedBodyResponse{
		noBodyResponse: noBodyResponse{
			Args:    r.URL.Query(),
			Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
			Method:  r.Method,
			Origin:  h.getClientIP(r),
			URL:     getURL(r).String(),
		},
	}

	n, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp.BytesReceived = n
	writeJSON(http.StatusOK, w, resp)
}

// Gzip returns a gzipped response
func (h *HTTPBin) Gzip(w http.ResponseWriter, r *http.Request) {
	var (
		buf bytes.Buffer
		gzw = gzip.NewWriter(&buf)
	)
	mustMarshalJSON(gzw, &noBodyResponse{
		Args:    r.URL.Query(),
		Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
		Method:  r.Method,
		Origin:  h.getClientIP(r),
		Gzipped: true,
	})
	gzw.Close()

	body := buf.Bytes()
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// Deflate returns a gzipped response
func (h *HTTPBin) Deflate(w http.ResponseWriter, r *http.Request) {
	var (
		buf bytes.Buffer
		zw  = zlib.NewWriter(&buf)
	)
	mustMarshalJSON(zw, &noBodyResponse{
		Args:     r.URL.Query(),
		Headers:  getRequestHeaders(r, h.excludeHeadersProcessor),
		Method:   r.Method,
		Origin:   h.getClientIP(r),
		Deflated: true,
	})
	zw.Close()

	body := buf.Bytes()
	w.Header().Set("Content-Encoding", "deflate")
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// IP echoes the IP address of the incoming request
func (h *HTTPBin) IP(w http.ResponseWriter, r *http.Request) {
	writeJSON(http.StatusOK, w, &ipResponse{
		Origin: h.getClientIP(r),
	})
}

// UserAgent echoes the incoming User-Agent header
func (h *HTTPBin) UserAgent(w http.ResponseWriter, r *http.Request) {
	writeJSON(http.StatusOK, w, &userAgentResponse{
		UserAgent: r.Header.Get("User-Agent"),
	})
}

// Headers echoes the incoming request headers
func (h *HTTPBin) Headers(w http.ResponseWriter, r *http.Request) {
	writeJSON(http.StatusOK, w, &headersResponse{
		Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
	})
}

type statusCase struct {
	headers map[string]string
	body    []byte
}

func createSpecialCases(prefix string) map[int]*statusCase {
	statusRedirectHeaders := &statusCase{
		headers: map[string]string{
			"Location": prefix + "/redirect/1",
		},
	}
	statusNotAcceptableBody := []byte(`{
  "message": "Client did not request a supported media type",
  "accept": [
    "image/webp",
    "image/svg+xml",
    "image/jpeg",
    "image/png",
    "image/avif",
    "image/"
  ]
}
`)
	statusHTTP300body := fmt.Appendf(nil, `<!doctype html>
<head>
<title>Multiple Choices</title>
</head>
<body>
<ul>
<li><a href="%[1]s/image/jpeg">/image/jpeg</a></li>
<li><a href="%[1]s/image/png">/image/png</a></li>
<li><a href="%[1]s/image/svg">/image/svg</a></li>
</body>
</html>`, prefix)

	statusHTTP308Body := fmt.Appendf(nil, `<!doctype html>
<head>
<title>Permanent Redirect</title>
</head>
<body>Permanently redirected to <a href="%[1]s/image/jpeg">%[1]s/image/jpeg</a>
</body>
</html>`, prefix)

	return map[int]*statusCase{
		300: {
			body: statusHTTP300body,
			headers: map[string]string{
				"Location": prefix + "/image/jpeg",
			},
		},
		301: statusRedirectHeaders,
		302: statusRedirectHeaders,
		303: statusRedirectHeaders,
		305: statusRedirectHeaders,
		307: statusRedirectHeaders,
		308: {
			body: statusHTTP308Body,
			headers: map[string]string{
				"Location": prefix + "/image/jpeg",
			},
		},
		401: {
			headers: map[string]string{
				"WWW-Authenticate": `Basic realm="Fake Realm"`,
			},
		},
		402: {
			body: []byte("Fuck you, pay me!"),
			headers: map[string]string{
				"X-More-Info": "http://vimeo.com/22053820",
			},
		},
		406: {
			body: statusNotAcceptableBody,
			headers: map[string]string{
				"Content-Type": jsonContentType,
			},
		},
		407: {
			headers: map[string]string{
				"Proxy-Authenticate": `Basic realm="Fake Realm"`,
			},
		},
		418: {
			body: []byte("I'm a teapot!"),
			headers: map[string]string{
				"X-More-Info": "http://tools.ietf.org/html/rfc2324",
			},
		},
		429: {
			headers: map[string]string{
				"Retry-After": "5",
			},
		},
	}
}

// Status responds with the specified status code. TODO: support random choice
// from multiple, optionally weighted status codes.
func (h *HTTPBin) Status(w http.ResponseWriter, r *http.Request) {
	rawStatus := r.PathValue("code")

	// simple case, specific status code is requested
	if !strings.Contains(rawStatus, ",") {
		code, err := parseStatusCode(rawStatus)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		h.doStatus(w, code)
		return
	}

	// complex case, make a weighted choice from multiple status codes
	choices, err := parseWeightedChoices(rawStatus, strconv.Atoi)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	choice := weightedRandomChoice(choices, rand.Float64)
	h.doStatus(w, choice)
}

func (h *HTTPBin) doStatus(w http.ResponseWriter, code int) {
	// default to plain text content type, which may be overriden by headers
	// for special cases
	w.Header().Set("Content-Type", textContentType)
	if specialCase, ok := h.statusSpecialCases[code]; ok {
		for key, val := range specialCase.headers {
			w.Header().Set(key, val)
		}
		w.WriteHeader(code)
		if specialCase.body != nil {
			w.Write(specialCase.body)
		}
		return
	}
	w.WriteHeader(code)
}

// Unstable - returns 500, sometimes
func (h *HTTPBin) Unstable(w http.ResponseWriter, r *http.Request) {
	var err error

	// rng/seed
	rng, err := parseSeed(r.URL.Query().Get("seed"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid seed: %w", err))
		return
	}

	// failure_rate
	failureRate := 0.5
	if rawFailureRate := r.URL.Query().Get("failure_rate"); rawFailureRate != "" {
		failureRate, err = strconv.ParseFloat(rawFailureRate, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid failure rate: %w", err))
			return
		} else if failureRate < 0 || failureRate > 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid failure rate: %d not in range [0, 1]", err))
			return
		}
	}

	status := http.StatusOK
	if rng.Float64() < failureRate {
		status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", textContentType)
	w.WriteHeader(status)
}

// ResponseHeaders sets every incoming query parameter as a response header and
// returns the headers serialized as JSON.
//
// If the Content-Type query parameter is given and set to a "dangerous" value
// (i.e. one that might be rendered as HTML in a web browser), the keys and
// values in the JSON response body will be escaped.
func (h *HTTPBin) ResponseHeaders(w http.ResponseWriter, r *http.Request) {
	args := r.URL.Query()

	// only set our own content type if one was not already set based on
	// incoming request params
	contentType := args.Get("Content-Type")
	if contentType == "" {
		contentType = jsonContentType
		args.Set("Content-Type", contentType)
	}

	// actual HTTP response headers are not escaped, regardless of content type
	// (unlike the JSON serialized representation of those headers in the
	// response body, which MAY be escaped based on content type)
	for k, vs := range args {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	// if response content type is dangrous, escape keys and values before
	// serializing response body
	if h.mustEscapeResponse(contentType) {
		tmp := make(url.Values, len(args))
		for k, vs := range args {
			for _, v := range vs {
				tmp.Add(html.EscapeString(k), html.EscapeString(v))
			}
		}
		args = tmp
	}

	mustMarshalJSON(w, args)
}

func (h *HTTPBin) redirectLocation(r *http.Request, relative bool, n int) string {
	var location string
	var path string

	if n < 1 {
		path = "/get"
	} else if relative {
		path = fmt.Sprintf("/relative-redirect/%d", n)
	} else {
		path = fmt.Sprintf("/absolute-redirect/%d", n)
	}

	if relative {
		location = path
	} else {
		u := getURL(r)
		u.Path = path
		u.RawQuery = ""
		location = u.String()
	}

	return location
}

func (h *HTTPBin) handleRedirect(w http.ResponseWriter, r *http.Request, relative bool) {
	n, err := strconv.Atoi(r.PathValue("numRedirects"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid redirect count: %w", err))
		return
	} else if n < 1 {
		writeError(w, http.StatusBadRequest, errors.New("redirect count must be > 0"))
		return
	}
	h.doRedirect(w, h.redirectLocation(r, relative, n-1), http.StatusFound)
}

// Redirect responds with 302 redirect a given number of times. Defaults to a
// relative redirect, but an ?absolute=true query param will trigger an
// absolute redirect.
func (h *HTTPBin) Redirect(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	relative := strings.ToLower(params.Get("absolute")) != "true"
	h.handleRedirect(w, r, relative)
}

// RelativeRedirect responds with an HTTP 302 redirect a given number of times
func (h *HTTPBin) RelativeRedirect(w http.ResponseWriter, r *http.Request) {
	h.handleRedirect(w, r, true)
}

// AbsoluteRedirect responds with an HTTP 302 redirect a given number of times
func (h *HTTPBin) AbsoluteRedirect(w http.ResponseWriter, r *http.Request) {
	h.handleRedirect(w, r, false)
}

// RedirectTo responds with a redirect to a specific URL with an optional
// status code, which defaults to 302
func (h *HTTPBin) RedirectTo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	inputURL := q.Get("url")
	if inputURL == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing required query parameter: url"))
		return
	}

	u, err := url.Parse(inputURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid url: %w", err))
		return
	}

	// If we're given a URL that includes a domain name and we have a list of
	// allowed domains, ensure that the domain is allowed.
	//
	// Note: This checks the hostname directly rather than using the net.URL's
	// IsAbs() method, because IsAbs() will return false for URLs that omit
	// the scheme but include a domain name, like "//evil.com" and it's
	// important that we validate the domain in these cases as well.
	if u.Hostname() != "" && len(h.AllowedRedirectDomains) > 0 {
		if _, ok := h.AllowedRedirectDomains[u.Hostname()]; !ok {
			// for this error message we do not use our standard JSON response
			// because we want it to be more obviously human readable.
			writeResponse(w, http.StatusForbidden, "text/plain", []byte(h.forbiddenRedirectError))
			return
		}
	}

	statusCode := http.StatusFound
	if userStatusCode := q.Get("status_code"); userStatusCode != "" {
		statusCode, err = parseBoundedStatusCode(userStatusCode, 300, 399)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}

	h.doRedirect(w, u.String(), statusCode)
}

// Cookies responds with the cookies in the incoming request
func (h *HTTPBin) Cookies(w http.ResponseWriter, r *http.Request) {
	resp := cookiesResponse{Cookies: make(map[string]string)}
	for _, c := range r.Cookies() {
		resp.Cookies[c.Name] = c.Value
	}
	writeJSON(http.StatusOK, w, resp)
}

// SetCookies sets cookies as specified in query params and redirects to
// Cookies endpoint
func (h *HTTPBin) SetCookies(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	for k := range params {
		http.SetCookie(w, &http.Cookie{
			Name:     k,
			Value:    params.Get(k),
			HttpOnly: true,
		})
	}
	h.doRedirect(w, "/cookies", http.StatusFound)
}

// DeleteCookies deletes cookies specified in query params and redirects to
// Cookies endpoint
func (h *HTTPBin) DeleteCookies(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	for k := range params {
		http.SetCookie(w, &http.Cookie{
			Name:     k,
			Value:    params.Get(k),
			HttpOnly: true,
			MaxAge:   -1,
			Expires:  time.Now().Add(-1 * 24 * 365 * time.Hour),
		})
	}
	h.doRedirect(w, "/cookies", http.StatusFound)
}

// BasicAuth requires basic authentication
func (h *HTTPBin) BasicAuth(w http.ResponseWriter, r *http.Request) {
	expectedUser := r.PathValue("user")
	expectedPass := r.PathValue("password")

	givenUser, givenPass, _ := r.BasicAuth()

	status := http.StatusOK
	authorized := givenUser == expectedUser && givenPass == expectedPass
	if !authorized {
		status = http.StatusUnauthorized
		w.Header().Set("WWW-Authenticate", `Basic realm="Fake Realm"`)
	}

	writeJSON(status, w, authResponse{
		Authenticated: authorized,
		Authorized:    authorized,
		User:          givenUser,
	})
}

// HiddenBasicAuth requires HTTP Basic authentication but returns a status of
// 404 if the request is unauthorized
func (h *HTTPBin) HiddenBasicAuth(w http.ResponseWriter, r *http.Request) {
	expectedUser := r.PathValue("user")
	expectedPass := r.PathValue("password")

	givenUser, givenPass, _ := r.BasicAuth()

	authorized := givenUser == expectedUser && givenPass == expectedPass
	if !authorized {
		writeError(w, http.StatusNotFound, nil)
		return
	}

	writeJSON(http.StatusOK, w, authResponse{
		Authenticated: authorized,
		Authorized:    authorized,
		User:          givenUser,
	})
}

// Stream responds with max(n, 100) lines of JSON-encoded request data.
func (h *HTTPBin) Stream(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("numLines"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count: %w", err))
		return
	}

	if n > 100 {
		n = 100
	} else if n < 1 {
		n = 1
	}

	resp := &streamResponse{
		Args:    r.URL.Query(),
		Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
		Origin:  h.getClientIP(r),
		URL:     getURL(r).String(),
	}

	f := w.(http.Flusher)
	for i := 0; i < n; i++ {
		resp.ID = i
		// Call json.Marshal directly to avoid pretty printing
		line, _ := json.Marshal(resp)
		w.Write(append(line, '\n'))
		f.Flush()
	}
}

// set of keys that may not be specified in trailers, per
// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Trailer#directives
var forbiddenTrailers = map[string]struct{}{
	http.CanonicalHeaderKey("Authorization"):     {},
	http.CanonicalHeaderKey("Cache-Control"):     {},
	http.CanonicalHeaderKey("Content-Encoding"):  {},
	http.CanonicalHeaderKey("Content-Length"):    {},
	http.CanonicalHeaderKey("Content-Range"):     {},
	http.CanonicalHeaderKey("Content-Type"):      {},
	http.CanonicalHeaderKey("Host"):              {},
	http.CanonicalHeaderKey("Max-Forwards"):      {},
	http.CanonicalHeaderKey("Set-Cookie"):        {},
	http.CanonicalHeaderKey("TE"):                {},
	http.CanonicalHeaderKey("Trailer"):           {},
	http.CanonicalHeaderKey("Transfer-Encoding"): {},
}

// Trailers adds the header keys and values specified in the request's query
// parameters as HTTP trailers in the response.
//
// Trailers are returned in canonical form. Any forbidden trailer will result
// in an error.
func (h *HTTPBin) Trailers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// ensure all requested trailers are allowed
	for k := range q {
		if _, found := forbiddenTrailers[http.CanonicalHeaderKey(k)]; found {
			writeError(w, http.StatusBadRequest, fmt.Errorf("forbidden trailer: %s", k))
			return
		}
	}
	for k := range q {
		w.Header().Add("Trailer", k)
	}
	h.RequestWithBody(w, r)
	w.(http.Flusher).Flush() // force chunked transfer encoding even when no trailers are given
	for k, vs := range q {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
}

// Delay waits for a given amount of time before responding, where the time may
// be specified as a golang-style duration or seconds in floating point. A range
// like "2-8" or "500ms-2s" is also supported, in which case a random delay
// within the range is chosen.
func (h *HTTPBin) Delay(w http.ResponseWriter, r *http.Request) {
	minDelay, maxDelay, err := parseDelayRange(r.PathValue("duration"), h.MaxDuration)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid duration: %w", err))
		return
	}

	delay := minDelay
	if maxDelay > minDelay {
		delay = minDelay + time.Duration(rand.Int63n(int64(maxDelay-minDelay+1)))
	}

	select {
	case <-r.Context().Done():
		w.WriteHeader(499) // "Client Closed Request" https://httpstatuses.com/499
		return
	case <-time.After(delay):
	}
	w.Header().Set("Server-Timing", encodeServerTimings([]serverTiming{
		{"initial_delay", delay, "initial delay"},
	}))
	h.RequestWithBody(w, r)
}

// Drip simulates a slow HTTP server by writing data over a given duration
// after an optional initial delay.
//
// Because this endpoint is intended to simulate a slow HTTP connection, it
// intentionally does NOT use chunked transfer encoding even though its
// implementation writes the response incrementally.
//
// See Stream (/stream) or StreamBytes (/stream-bytes) for endpoints that
// respond using chunked transfer encoding.
func (h *HTTPBin) Drip(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var (
		duration = h.DefaultParams.DripDuration
		delay    = h.DefaultParams.DripDelay
		numBytes = h.DefaultParams.DripNumBytes
		code     = http.StatusOK

		err error
	)

	if userDuration := q.Get("duration"); userDuration != "" {
		duration, err = parseBoundedDuration(userDuration, 0, h.MaxDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid duration: %w", err))
			return
		}
	}

	if userDelay := q.Get("delay"); userDelay != "" {
		delay, err = parseBoundedDuration(userDelay, 0, h.MaxDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid delay: %w", err))
			return
		}
	}

	if userNumBytes := q.Get("numbytes"); userNumBytes != "" {
		numBytes, err = strconv.ParseInt(userNumBytes, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid numbytes: %w", err))
			return
		} else if numBytes < 1 || numBytes > h.MaxBodySize {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid numbytes: %d not in range [1, %d]", numBytes, h.MaxBodySize))
			return
		}
	}

	if userCode := q.Get("code"); userCode != "" {
		code, err = parseStatusCode(userCode)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}

	if duration+delay > h.MaxDuration {
		writeError(w, http.StatusBadRequest, fmt.Errorf("too much time: %v+%v > %v", duration, delay, h.MaxDuration))
		return
	}

	pause := computePausePerWrite(duration, numBytes)

	// Initial delay before we send any response data
	if delay > 0 {
		select {
		case <-time.After(delay):
			// ok
		case <-r.Context().Done():
			w.WriteHeader(499) // "Client Closed Request" https://httpstatuses.com/499
			return
		}
	}

	w.Header().Set("Content-Type", textContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", numBytes))
	w.Header().Set("Server-Timing", encodeServerTimings([]serverTiming{
		{"total_duration", delay + duration, "total request duration"},
		{"initial_delay", delay, "initial delay"},
		{"write_duration", duration, "duration of writes after initial delay"},
		{"pause_per_write", pause, "computed pause between writes"},
	}))
	w.WriteHeader(code)

	// what we write with each increment of the ticker
	b := []byte{'*'}

	// special case when we do not need to pause between each write
	if pause == 0 {
		w.Write(bytes.Repeat(b, int(numBytes)))
		return
	}

	// otherwise, write response body byte-by-byte
	ticker := time.NewTicker(pause)
	defer ticker.Stop()

	flusher := w.(http.Flusher)
	for i := int64(0); i < numBytes; i++ {
		w.Write(b)
		flusher.Flush()

		// don't pause after last byte
		if i == numBytes-1 {
			return
		}

		select {
		case <-ticker.C:
			// ok
		case <-r.Context().Done():
			return
		}
	}
}

// Range returns up to N bytes, with support for HTTP Range requests.
//
// This departs from original httpbin in a few ways:
//
//   - param `chunk_size` IS NOT supported
//
//   - param `duration` IS supported, but functions more as a delay before the
//     whole response is written
//
//   - multiple ranges ARE correctly supported (i.e. `Range: bytes=0-1,2-3`
//     will return a multipart/byteranges response)
//
// Most of the heavy lifting is done by the stdlib's http.ServeContent, which
// handles range requests automatically. Supporting chunk sizes would require
// an extensive reimplementation, especially to support multiple ranges for
// correctness. For now, we choose not to take that work on.
func (h *HTTPBin) Range(w http.ResponseWriter, r *http.Request) {
	numBytes, err := strconv.ParseInt(r.PathValue("numBytes"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count: %w", err))
		return
	}

	var duration time.Duration
	if durationVal := r.URL.Query().Get("duration"); durationVal != "" {
		var err error
		duration, err = parseBoundedDuration(r.URL.Query().Get("duration"), 0, h.MaxDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid duration: %w", err))
			return
		}
	}

	w.Header().Add("ETag", fmt.Sprintf("range%d", numBytes))
	w.Header().Add("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", textContentType)

	if numBytes <= 0 || numBytes > h.MaxBodySize {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count: %d not in range [1, %d]", numBytes, h.MaxBodySize))
		return
	}

	content := newSyntheticByteStream(numBytes, duration, func(offset int64) byte {
		return byte(97 + (offset % 26))
	})
	var modtime time.Time
	http.ServeContent(w, r, "", modtime, content)
}

// HTML renders a basic HTML page
func (h *HTTPBin) HTML(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, mustStaticAsset("moby.html"), http.StatusOK)
}

// Robots renders a basic robots.txt file
func (h *HTTPBin) Robots(w http.ResponseWriter, _ *http.Request) {
	robotsTxt := []byte(`User-agent: *
Disallow: /deny
`)
	writeResponse(w, http.StatusOK, textContentType, robotsTxt)
}

// Deny renders a basic page that robots should never access
func (h *HTTPBin) Deny(w http.ResponseWriter, _ *http.Request) {
	writeResponse(w, http.StatusOK, textContentType, []byte(`YOU SHOULDN'T BE HERE`))
}

// Cache returns a 304 if an If-Modified-Since or an If-None-Match header is
// present, otherwise returns the same response as Get.
func (h *HTTPBin) Cache(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-None-Match") != "" {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	lastModified := time.Now().Format(time.RFC1123)
	w.Header().Add("Last-Modified", lastModified)
	w.Header().Add("ETag", sha1hash(lastModified))
	h.Get(w, r)
}

// CacheControl sets a Cache-Control header for N seconds for /cache/N requests
func (h *HTTPBin) CacheControl(w http.ResponseWriter, r *http.Request) {
	seconds, err := strconv.ParseInt(r.PathValue("numSeconds"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid seconds: %w", err))
		return
	}
	w.Header().Add("Cache-Control", fmt.Sprintf("public, max-age=%d", seconds))
	h.Get(w, r)
}

// ETag assumes the resource has the given etag and responds to If-None-Match
// and If-Match headers appropriately.
func (h *HTTPBin) ETag(w http.ResponseWriter, r *http.Request) {
	etag := r.PathValue("etag")
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("Content-Type", textContentType)

	var buf bytes.Buffer
	mustMarshalJSON(&buf, noBodyResponse{
		Args:    r.URL.Query(),
		Headers: getRequestHeaders(r, h.excludeHeadersProcessor),
		Method:  r.Method,
		Origin:  h.getClientIP(r),
		URL:     getURL(r).String(),
	})

	// Let http.ServeContent deal with If-None-Match and If-Match headers:
	// https://golang.org/pkg/net/http/#ServeContent
	http.ServeContent(w, r, "response.json", time.Now(), bytes.NewReader(buf.Bytes()))
}

// Bytes returns N random bytes generated with an optional seed
func (h *HTTPBin) Bytes(w http.ResponseWriter, r *http.Request) {
	h.handleBytes(w, r, false)
}

// StreamBytes streams N random bytes generated with an optional seed in chunks
// of a given size.
func (h *HTTPBin) StreamBytes(w http.ResponseWriter, r *http.Request) {
	h.handleBytes(w, r, true)
}

// handleBytes consolidates the logic for validating input params of the Bytes
// and StreamBytes endpoints and knows how to write the response in chunks if
// streaming is true.
func (h *HTTPBin) handleBytes(w http.ResponseWriter, r *http.Request, streaming bool) {
	numBytes, err := strconv.Atoi(r.PathValue("numBytes"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid byte count: %w", err))
		return
	}

	// rng/seed
	rng, err := parseSeed(r.URL.Query().Get("seed"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid seed: %w", err))
		return
	}

	if numBytes < 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid byte count: %d must be greater than 0", numBytes))
		return
	}

	// Special case 0 bytes and exit early, since streaming & chunk size do not
	// matter here.
	if numBytes == 0 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}

	if numBytes > int(h.MaxBodySize) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid byte count: %d not in range [1, %d]", numBytes, h.MaxBodySize))
		return
	}

	var chunkSize int
	var write func([]byte)

	if streaming {
		if r.URL.Query().Get("chunk_size") != "" {
			chunkSize, err = strconv.Atoi(r.URL.Query().Get("chunk_size"))
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid chunk_size: %w", err))
				return
			}
		} else {
			chunkSize = 10 * 1024
		}

		write = func() func(chunk []byte) {
			f := w.(http.Flusher)
			return func(chunk []byte) {
				w.Write(chunk)
				f.Flush()
			}
		}()
	} else {
		// if not streaming, we will write the whole response at once
		chunkSize = numBytes
		w.Header().Set("Content-Length", strconv.Itoa(numBytes))
		write = func(chunk []byte) {
			w.Write(chunk)
		}
	}

	w.Header().Set("Content-Type", binaryContentType)
	w.WriteHeader(http.StatusOK)

	var chunk []byte
	for range numBytes {
		chunk = append(chunk, byte(rng.Intn(256)))
		if len(chunk) == chunkSize {
			write(chunk)
			chunk = nil
		}
	}
	if len(chunk) > 0 {
		write(chunk)
	}
}

// Links redirects to the first page in a series of N links
func (h *HTTPBin) Links(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("numLinks"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid link count: %w", err))
		return
	} else if n < 0 || n > 256 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid link count: %d must be in range [0, 256]", n))
		return
	}

	// Are we handling /links/<n>/<offset>? If so, render an HTML page
	if rawOffset := r.PathValue("offset"); rawOffset != "" {
		offset, err := strconv.Atoi(rawOffset)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid offset: %w", err))
			return
		}
		h.doLinksPage(w, r, n, offset)
		return
	}

	// Otherwise, redirect from /links/<n> to /links/<n>/0
	r.URL.Path = r.URL.Path + "/0"
	h.doRedirect(w, r.URL.String(), http.StatusFound)
}

// doLinksPage renders a page with a series of N links
func (h *HTTPBin) doLinksPage(w http.ResponseWriter, _ *http.Request, n int, offset int) {
	w.Header().Add("Content-Type", htmlContentType)
	w.WriteHeader(http.StatusOK)

	w.Write([]byte("<html><head><title>Links</title></head><body>"))
	for i := range n {
		if i == offset {
			fmt.Fprintf(w, "%d ", i)
		} else {
			fmt.Fprintf(w, `<a href="%s/links/%d/%d">%d</a> `, h.prefix, n, i, i)
		}
	}
	w.Write([]byte("</body></html>"))
}

// doRedirect set redirect header
func (h *HTTPBin) doRedirect(w http.ResponseWriter, path string, code int) {
	var sb strings.Builder
	if strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
		sb.WriteString(h.prefix)
	}
	sb.WriteString(path)
	w.Header().Set("Location", sb.String())
	w.WriteHeader(code)
}

// ImageAccept responds with an appropriate image based on the Accept header
func (h *HTTPBin) ImageAccept(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	var kind string
	switch {
	case accept == "":
		kind = "png"
	case strings.Contains(accept, "image/*"):
		kind = "png"
	case strings.Contains(accept, "image/png"):
		kind = "png"
	case strings.Contains(accept, "image/webp"):
		kind = "webp"
	case strings.Contains(accept, "image/svg+xml"):
		kind = "svg"
	case strings.Contains(accept, "image/jpeg"):
		kind = "jpeg"
	case strings.Contains(accept, "image/avif"):
		kind = "avif"
	default:
		writeError(w, http.StatusUnsupportedMediaType, nil)
		return
	}

	// If size or gradient parameter is present, delegate to Image handler logic
	params := r.URL.Query()
	if params.Get("size") != "" || params.Get("gradient") != "" || params.Get("color1") != "" {
		r.SetPathValue("kind", kind)
		h.Image(w, r)
		return
	}

	doImage(w, kind)
}

// imageExtToHandlerKind maps file extensions to image kind names for vanity URLs.
var imageExtToHandlerKind = map[string]string{
	".png": "png", ".jpeg": "jpeg", ".jpg": "jpeg",
	".svg": "svg", ".webp": "webp", ".avif": "avif",
}

// Image responds with an image of a specific kind, from /image/<kind>
func (h *HTTPBin) Image(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")

	// Support vanity URLs like /image/photo.png → kind=png
	if ext := strings.ToLower(path.Ext(kind)); ext != "" {
		if resolved, ok := imageExtToHandlerKind[ext]; ok {
			kind = resolved
			r.SetPathValue("kind", kind)
		}
	}

	params := r.URL.Query()

	// Parse gradient configuration
	grad, err := parseGradientConfig(params)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Parse nocache flag
	nocache := params.Get("nocache") == "1"

	// Check for size parameter
	sizeParam := params.Get("size")
	if sizeParam != "" {
		var targetSize int
		switch sizeParam {
		case "small":
			targetSize = 50 * 1024 // ~50KB
		case "medium":
			targetSize = 500 * 1024 // ~500KB
		case "large":
			targetSize = 2 * 1024 * 1024 // ~2MB
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid size %q: must be small, medium, or large", sizeParam))
			return
		}

		// Determine the base format for generation
		genFormat := kind
		if kind == "avif" || kind == "webp" {
			genFormat = "png" // generate PNG then convert
		}

		switch genFormat {
		case "png", "jpeg":
			// Use cache for deterministic requests
			seed := int64(42)
			if nocache {
				seed = time.Now().UnixNano()
			}

			var data []byte
			var contentType string

			cacheKey := imageCacheKey{format: kind, targetSize: targetSize, grad: grad}

			if !nocache {
				if entry, ok := h.imgCache.get(cacheKey); ok {
					w.Header().Set("Cache-Control", "public, max-age=86400")
					w.Header().Set("ETag", entry.etag)
					writeResponse(w, http.StatusOK, entry.contentType, entry.data)
					return
				}
			}

			data, contentType, err = generateImage(genFormat, targetSize, grad, seed)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			// Convert to AVIF/WebP if needed
			if kind == "avif" || kind == "webp" {
				conv, ok := h.imageConverters[kind]
				if !ok {
					writeError(w, http.StatusNotImplemented, fmt.Errorf("no %s conversion tool available; install avifenc/cwebp/magick/ffmpeg", kind))
					return
				}
				converted, convErr := convertImageFormat(r.Context(), data, conv)
				if convErr != nil {
					writeError(w, http.StatusInternalServerError, convErr)
					return
				}
				data = converted
				contentType = "image/" + kind
			}

			if nocache {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			} else {
				etag := fmt.Sprintf(`"%s"`, sha1hashBytes(data))
				h.imgCache.put(cacheKey, imageCacheEntry{data: data, contentType: contentType, etag: etag})
				w.Header().Set("Cache-Control", "public, max-age=86400")
				w.Header().Set("ETag", etag)
			}
			writeResponse(w, http.StatusOK, contentType, data)
			return
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf("size parameter only supported for png, jpeg, avif, and webp, not %s", kind))
			return
		}
	}

	doImage(w, kind)
}

// doImage responds with a specific kind of image, if there is an image asset
// of the given kind.
func doImage(w http.ResponseWriter, kind string) {
	img, err := staticAsset("image." + kind)
	if err != nil {
		writeError(w, http.StatusNotFound, nil)
		return
	}
	contentType := "image/" + kind
	if kind == "svg" {
		contentType = "image/svg+xml"
	}
	writeResponse(w, http.StatusOK, contentType, img)
}

// XML responds with an XML document
func (h *HTTPBin) XML(w http.ResponseWriter, _ *http.Request) {
	writeResponse(w, http.StatusOK, "application/xml", mustStaticAsset("sample.xml"))
}

// DigestAuth handles a simple implementation of HTTP Digest Authentication,
// which supports the "auth" QOP and the MD5 and SHA-256 crypto algorithms.
//
// /digest-auth/<qop>/<user>/<passwd>
// /digest-auth/<qop>/<user>/<passwd>/<algorithm>
func (h *HTTPBin) DigestAuth(w http.ResponseWriter, r *http.Request) {
	var (
		qop      = strings.ToLower(r.PathValue("qop"))
		user     = r.PathValue("user")
		password = r.PathValue("password")
		algoName = strings.ToUpper(r.PathValue("algorithm"))
	)
	if algoName == "" {
		algoName = "MD5"
	}

	if qop != "auth" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid QOP directive: %q != \"auth\"", qop))
		return
	}
	if algoName != "MD5" && algoName != "SHA-256" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid algorithm: %s must be one of MD5 or SHA-256", algoName))
		return
	}

	algorithm := digest.MD5
	if algoName == "SHA-256" {
		algorithm = digest.SHA256
	}

	if !digest.Check(r, user, password) {
		w.Header().Set("WWW-Authenticate", digest.Challenge("go-httpbin", algorithm))
		writeError(w, http.StatusUnauthorized, nil)
		return
	}

	writeJSON(http.StatusOK, w, authResponse{
		Authenticated: true,
		Authorized:    true,
		User:          user,
	})
}

// UUID - responds with a generated UUID
func (h *HTTPBin) UUID(w http.ResponseWriter, _ *http.Request) {
	writeJSON(http.StatusOK, w, uuidResponse{
		UUID: uuidv4(),
	})
}

// Base64 - encodes/decodes input data
func (h *HTTPBin) Base64(w http.ResponseWriter, r *http.Request) {
	result, err := newBase64Helper(r, h.MaxBodySize).transform()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ct := r.URL.Query().Get("content-type")
	if ct == "" {
		ct = textContentType
	}
	// prevent XSS and other client side vulns if the content type is dangerous
	if h.mustEscapeResponse(ct) {
		result = []byte(html.EscapeString(string(result)))
	}
	writeResponse(w, http.StatusOK, ct, result)
}

// DumpRequest - returns the given request in its HTTP/1.x wire representation.
// The returned representation is an approximation only;
// some details of the initial request are lost while parsing it into
// an http.Request. In particular, the order and case of header field
// names are lost.
func (h *HTTPBin) DumpRequest(w http.ResponseWriter, r *http.Request) {
	dump, err := httputil.DumpRequest(r, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to dump request: %w", err))
		return
	}
	w.Write(dump)
}

// JSON - returns a sample json
func (h *HTTPBin) JSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusOK)
	w.Write(mustStaticAsset("sample.json"))
}

// Bearer - Prompts the user for authorization using bearer authentication.
func (h *HTTPBin) Bearer(w http.ResponseWriter, r *http.Request) {
	reqToken := r.Header.Get("Authorization")
	tokenFields := strings.Fields(reqToken)
	if len(tokenFields) != 2 || tokenFields[0] != "Bearer" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, nil)
		return
	}
	writeJSON(http.StatusOK, w, bearerResponse{
		Authenticated: true,
		Token:         tokenFields[1],
	})
}

// Hostname - returns the hostname.
func (h *HTTPBin) Hostname(w http.ResponseWriter, _ *http.Request) {
	writeJSON(http.StatusOK, w, hostnameResponse{
		Hostname: h.hostname,
	})
}

// SSE writes a stream of events over a duration after an optional
// initial delay.
func (h *HTTPBin) SSE(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := r.URL.Query()
	var (
		count       = h.DefaultParams.SSECount
		duration    = h.DefaultParams.SSEDuration
		delay       = h.DefaultParams.SSEDelay
		eventName   string
		retryMS     int
		failAfter   int
		lastEventID int
		err         error
	)

	if userCount := q.Get("count"); userCount != "" {
		count, err = strconv.Atoi(userCount)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count: %w", err))
			return
		}
		if count < 1 || int64(count) > h.maxSSECount {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid count: must in range [1, %d]", h.maxSSECount))
			return
		}
	}

	if userDuration := q.Get("duration"); userDuration != "" {
		duration, err = parseBoundedDuration(userDuration, 1, h.MaxDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid duration: %w", err))
			return
		}
	}

	if userDelay := q.Get("delay"); userDelay != "" {
		delay, err = parseBoundedDuration(userDelay, 0, h.MaxDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid delay: %w", err))
			return
		}
	}

	if userEvent := q.Get("event"); userEvent != "" {
		eventName = strings.ReplaceAll(strings.ReplaceAll(userEvent, "\n", ""), "\r", "")
	}

	if userRetry := q.Get("retry"); userRetry != "" {
		retryMS, err = strconv.Atoi(userRetry)
		if err != nil || retryMS < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid retry: must be a non-negative integer"))
			return
		}
	}

	if userFailAfter := q.Get("fail_after"); userFailAfter != "" {
		failAfter, err = strconv.Atoi(userFailAfter)
		if err != nil || failAfter < 1 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid fail_after: must be a positive integer"))
			return
		}
	}

	if rawLastEventID := r.Header.Get("Last-Event-ID"); rawLastEventID != "" {
		lastEventID, err = strconv.Atoi(rawLastEventID)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid Last-Event-ID: %w", err))
			return
		}
	}

	if duration+delay > h.MaxDuration {
		http.Error(w, "Too much time", http.StatusBadRequest)
		return
	}

	pause := computePausePerWrite(duration, int64(count))

	// Initial delay before we send any response data
	if delay > 0 {
		select {
		case <-time.After(delay):
			// ok
		case <-r.Context().Done():
			w.WriteHeader(499) // "Client Closed Request" https://httpstatuses.com/499
			return
		}
	}

	w.Header().Add("Trailer", "Server-Timing")
	defer func() {
		w.Header().Add("Server-Timing", encodeServerTimings([]serverTiming{
			{"total_duration", time.Since(start), "total request duration"},
			{"initial_delay", delay, "initial delay"},
			{"write_duration", duration, "duration of writes after initial delay"},
			{"pause_per_write", pause, "computed pause between writes"},
		}))
	}()
	w.Header().Set("Content-Type", sseContentType)
	w.WriteHeader(http.StatusOK)

	flusher := w.(http.Flusher)

	// Write retry directive at the start of the stream if requested
	if retryMS > 0 {
		fmt.Fprintf(w, "retry: %d\n\n", retryMS)
		flusher.Flush()
	}

	// Event IDs start after lastEventID (for SSE reconnection semantics)
	startID := lastEventID + 1

	// special case when we only have one event to write
	if count == 1 {
		writeServerSentEvent(w, startID, time.Now(), eventName)
		flusher.Flush()
		return
	}

	ticker := time.NewTicker(pause)
	defer ticker.Stop()

	for i := 0; i < count; i++ {
		if failAfter > 0 && i >= failAfter {
			return
		}

		writeServerSentEvent(w, startID+i, time.Now(), eventName)
		flusher.Flush()

		// don't pause after last byte
		if i == count-1 {
			return
		}

		select {
		case <-ticker.C:
			// ok
		case <-r.Context().Done():
			return
		}
	}
}

// writeServerSentEvent writes the bytes that constitute a single server-sent
// event message, including the event ID, optional event type, and data.
func writeServerSentEvent(dst io.Writer, id int, ts time.Time, eventName string) {
	fmt.Fprintf(dst, "id: %d\n", id)
	if eventName != "" {
		fmt.Fprintf(dst, "event: %s\n", strings.ReplaceAll(strings.ReplaceAll(eventName, "\n", ""), "\r", ""))
	} else {
		dst.Write([]byte("event: ping\n"))
	}
	dst.Write([]byte("data: "))
	json.NewEncoder(dst).Encode(serverSentEvent{
		ID:        id,
		Timestamp: ts.UnixMilli(),
	})
	// each SSE ends with two newlines (\n\n), the first of which is written
	// automatically by json.NewEncoder().Encode()
	dst.Write([]byte("\n"))
}

// WebSocketEcho - simple websocket echo server, where the max fragment size
// and max message size can be controlled by clients.
func (h *HTTPBin) WebSocketEcho(w http.ResponseWriter, r *http.Request) {
	var (
		maxFragmentSize = h.MaxBodySize / 2
		maxMessageSize  = h.MaxBodySize
		q               = r.URL.Query()
		err             error
	)

	if userMaxFragmentSize := q.Get("max_fragment_size"); userMaxFragmentSize != "" {
		maxFragmentSize, err = strconv.ParseInt(userMaxFragmentSize, 10, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid max_fragment_size: %w", err))
			return
		} else if maxFragmentSize < 1 || maxFragmentSize > h.MaxBodySize {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid max_fragment_size: %d not in range [1, %d]", maxFragmentSize, h.MaxBodySize))
			return
		}
	}

	if userMaxMessageSize := q.Get("max_message_size"); userMaxMessageSize != "" {
		maxMessageSize, err = strconv.ParseInt(userMaxMessageSize, 10, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid max_message_size: %w", err))
			return
		} else if maxMessageSize < 1 || maxMessageSize > h.MaxBodySize {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid max_message_size: %d not in range [1, %d]", maxMessageSize, h.MaxBodySize))
			return
		}
	}

	if maxFragmentSize > maxMessageSize {
		writeError(w, http.StatusBadRequest, fmt.Errorf("max_fragment_size %d must be less than or equal to max_message_size %d", maxFragmentSize, maxMessageSize))
		return
	}

	ws := websocket.New(w, r, websocket.Limits{
		MaxDuration:     h.MaxDuration,
		MaxFragmentSize: int(maxFragmentSize),
		MaxMessageSize:  int(maxMessageSize),
	})
	if err := ws.Handshake(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ws.Serve(websocket.EchoHandler)
}

// Version returns the version of the go-httpbin instance and the Go runtime.
func (h *HTTPBin) Version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(http.StatusOK, w, &versionResponse{
		Version:   h.version,
		GoVersion: runtime.Version(),
	})
}

// PDF returns a dynamically generated PDF document.
// Supports query parameters: pages (1-100), size (small/medium/large), seed (int), nocache (1).
func (h *HTTPBin) PDF(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	pages := 1
	if rawPages := q.Get("pages"); rawPages != "" {
		var err error
		pages, err = strconv.Atoi(rawPages)
		if err != nil || pages < 1 || pages > 100 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid pages value %q: must be 1-100", rawPages))
			return
		}
	}

	size := "medium"
	if rawSize := q.Get("size"); rawSize != "" {
		switch rawSize {
		case "small", "medium", "large":
			size = rawSize
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid size %q: must be small, medium, or large", rawSize))
			return
		}
	}

	nocache := q.Get("nocache") == "1"

	var seed int64 = 42
	if nocache {
		seed = time.Now().UnixNano()
	} else if rawSeed := q.Get("seed"); rawSeed != "" {
		var err error
		seed, err = parseSeedInt64(rawSeed)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid seed %q: %w", rawSeed, err))
			return
		}
	}

	cacheKey := pdfCacheKey{pages: pages, size: size, seed: seed}

	// Check cache for deterministic requests
	if !nocache {
		if entry, ok := h.pdfCache.get(cacheKey); ok {
			writeResponse(w, http.StatusOK, "application/pdf", entry.data)
			return
		}
	}

	data, err := generatePDF(pages, size, seed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if !nocache {
		h.pdfCache.put(cacheKey, pdfCacheEntry{data: data})
	}

	writeResponse(w, http.StatusOK, "application/pdf", data)
}

// ProblemDetails returns a Problem Details (RFC 9457) JSON response.
func (h *HTTPBin) ProblemDetails(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	status := http.StatusOK
	if rawStatus := q.Get("status"); rawStatus != "" {
		var err error
		status, err = parseStatusCode(rawStatus)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	} else if ow, ok := w.(interface{ StatusOverride() int }); ok {
		status = ow.StatusOverride()
	}

	resp := &problemDetailResponse{
		Type:     q.Get("type"),
		Title:    q.Get("title"),
		Status:   status,
		Detail:   q.Get("detail"),
		Instance: q.Get("instance"),
	}
	if resp.Type == "" {
		resp.Type = "about:blank"
	}
	if resp.Title == "" {
		resp.Title = http.StatusText(status)
	}

	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(status)
	mustMarshalJSON(w, resp)
}

// Echo returns the request body back as the response, preserving the
// Content-Type.
func (h *HTTPBin) Echo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("error reading request body: %w", err))
		return
	}
	defer r.Body.Close()

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = binaryContentType
	}

	// Escape dangerous content types to prevent XSS
	if h.mustEscapeResponse(ct) {
		ct = textContentType
		body = []byte(html.EscapeString(string(body)))
	}

	writeResponse(w, http.StatusOK, ct, body)
}

// Close abruptly closes the TCP connection, optionally after sending headers
// or partial data.
func (h *HTTPBin) Close(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("server does not support hijacking"))
		return
	}

	q := r.URL.Query()
	mode := q.Get("mode")
	after := q.Get("after")

	conn, buf, err := hj.Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer conn.Close()

	switch after {
	case "headers":
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n")
		buf.Flush()
	case "partial":
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 1024\r\n\r\n")
		buf.WriteString("partial data...")
		buf.Flush()
	}

	if mode == "reset" {
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetLinger(0)
		}
	}
	// conn.Close() called by defer
}

var negotiateTypes = map[string]string{
	"application/json": jsonContentType,
	"text/html":        htmlContentType,
	"text/plain":       textContentType,
	"application/xml":  "application/xml; charset=utf-8",
	"image/png":        "image/png",
}

// Negotiate performs server-driven content negotiation based on the Accept
// header.
func (h *HTTPBin) Negotiate(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	entries := parseAcceptHeader(accept)

	w.Header().Set("Vary", "Accept")

	// If no Accept header, default to JSON
	if len(entries) == 0 {
		writeJSON(http.StatusOK, w, map[string]string{"content_type": jsonContentType})
		return
	}

	for _, entry := range entries {
		if entry.mediaType == "*/*" {
			writeJSON(http.StatusOK, w, map[string]string{"content_type": jsonContentType})
			return
		}
		if ct, ok := negotiateTypes[entry.mediaType]; ok {
			writeJSON(http.StatusOK, w, map[string]string{"content_type": ct})
			return
		}
	}

	writeError(w, http.StatusNotAcceptable, fmt.Errorf("no matching content type for Accept: %s", accept))
}
