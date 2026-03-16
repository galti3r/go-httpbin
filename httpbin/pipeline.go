package httpbin

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// terminalDef describes a terminal endpoint in a pipeline URL.
type terminalDef struct {
	// Number of fixed arguments the terminal consumes after its name.
	// -1 means it consumes all remaining segments.
	args int

	// Names for path values to set on the request (e.g. ["code"] for /status).
	pathValues []string

	// HTTP method restriction ("GET", "POST", etc.) or "" for any method.
	method string
}

// pipelineStep represents a parsed modifier or terminal in the pipeline.
type pipelineStep struct {
	name string
	args []string
}

// pipelineResult is the output of parsePipeline.
type pipelineResult struct {
	modifiers []pipelineStep
	terminal  pipelineStep
}

// modifierDef describes a modifier with its argument count.
type modifierDef struct {
	args int // number of arguments the modifier consumes
}

// pipelineModifierDefs defines recognized modifiers.
var pipelineModifierDefs = map[string]modifierDef{
	"delay":          {args: 1},
	"response_delay": {args: 1},
	"status":         {args: 1}, // dual-role: modifier or terminal
	"header":         {args: 1}, // modifier only, arg=name:value
}

// pipelineTerminals maps terminal names to their definitions.
// Multi-segment names (e.g. "cookies/set") are checked via longest-prefix match.
var pipelineTerminals = map[string]terminalDef{
	// Multi-segment names (2 segments)
	"cookies/set":    {args: 0},
	"cookies/delete": {args: 0},
	"encoding/utf8":  {args: 0, method: "GET"},
	"forms/post":     {args: 0, method: "GET"},
	"dump/request":   {args: 0},

	// Single-segment names: method-restricted
	"get":    {args: 0, method: "GET"},
	"head":   {args: 0, method: "HEAD"},
	"post":   {args: 0, method: "POST"},
	"put":    {args: 0, method: "PUT"},
	"delete": {args: 0, method: "DELETE"},
	"patch":  {args: 0, method: "PATCH"},

	// Single-segment names: any method, fixed args
	"status":       {args: 1, pathValues: []string{"code"}},
	"bytes":        {args: 1, pathValues: []string{"numBytes"}},
	"stream":       {args: 1, pathValues: []string{"numLines"}},
	"stream-bytes": {args: 1, pathValues: []string{"numBytes"}},
	"etag":         {args: 1, pathValues: []string{"etag"}},
	"range":        {args: 1, pathValues: []string{"numBytes"}},
	"links":        {args: -1}, // 1 or 2 args
	"cache":        {args: -1}, // 0 or 1 args
	"base64":       {args: -1}, // 1 or 2 args

	// Auth terminals with fixed args
	"basic-auth":        {args: 2, pathValues: []string{"user", "password"}},
	"hidden-basic-auth": {args: 2, pathValues: []string{"user", "password"}},
	"digest-auth":       {args: -1}, // 3 or 4 args

	// Single-segment names: any method, no args
	"gzip":       {args: 0},
	"deflate":    {args: 0},
	"html":       {args: 0},
	"json":       {args: 0},
	"xml":        {args: 0},
	"robots.txt": {args: 0},
	"deny":       {args: 0},
	"ip":         {args: 0},
	"headers":    {args: 0},
	"user-agent": {args: 0},
	"uuid":       {args: 0},
	"hostname":   {args: 0},
	"bearer":     {args: 0},
	"cookies":    {args: 0},
	"drip":       {args: 0},
	"sse":        {args: 0},
	"unstable":   {args: 0},
	"anything":   {args: 0},
	"env":        {args: 0},
	"pdf":        {args: 0},
	"trailers":   {args: 0},

	// Body terminal (base64-encoded body content)
	"body": {args: 1, pathValues: []string{"data"}},

	// Variable-arg terminals (consume all remaining segments)
	"image":             {args: -1},
	"redirect":          {args: -1},
	"absolute-redirect": {args: -1},
	"relative-redirect": {args: -1},
}

const maxPipelineSegments = 20

// parsePipeline parses a URL path into modifiers and a terminal.
func parsePipeline(urlPath string) (*pipelineResult, error) {
	// Clean the path and split into non-empty segments
	cleaned := path.Clean(urlPath)
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return nil, fmt.Errorf("empty pipeline path")
	}

	var segments []string
	for _, s := range strings.Split(cleaned, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}

	if len(segments) == 0 {
		return nil, fmt.Errorf("empty pipeline path")
	}

	if len(segments) > maxPipelineSegments {
		return nil, fmt.Errorf("too many segments (max %d, got %d)", maxPipelineSegments, len(segments))
	}

	result := &pipelineResult{}
	i := 0

	for i < len(segments) {
		seg := segments[i]

		// Check if this segment is a modifier (with lookahead for dual-role)
		if modDef, isModifier := pipelineModifierDefs[seg]; isModifier {
			_, isTerminal := pipelineTerminals[seg]

			// Lookahead: treat as modifier if there are enough segments
			// after consuming its args for a terminal to follow.
			treatAsModifier := !isTerminal || (i+1+modDef.args) < len(segments)

			if treatAsModifier {
				// Modifier must have its value argument
				if i+1 >= len(segments) {
					return nil, fmt.Errorf("modifier %q requires a value", seg)
				}
				result.modifiers = append(result.modifiers, pipelineStep{
					name: seg,
					args: []string{segments[i+1]},
				})
				i += 2
				continue
			}
			// Fall through to terminal matching
		}

		// Not a modifier — try to match a terminal (longest-prefix first)
		remaining := segments[i:]

		// Try 2-segment match first
		if len(remaining) >= 2 {
			twoSeg := remaining[0] + "/" + remaining[1]
			if _, ok := pipelineTerminals[twoSeg]; ok {
				result.terminal = pipelineStep{
					name: twoSeg,
					args: nil,
				}
				// 2-segment terminals consume no additional args
				if len(remaining) > 2 {
					result.terminal.args = remaining[2:]
				}
				return result, nil
			}
		}

		// Try 1-segment match
		if def, ok := pipelineTerminals[remaining[0]]; ok {
			termArgs := remaining[1:]
			if def.args >= 0 && len(termArgs) > def.args {
				termArgs = remaining[1 : 1+def.args]
			}
			if def.args >= 0 && len(termArgs) < def.args {
				return nil, fmt.Errorf("terminal %q requires %d argument(s), got %d", remaining[0], def.args, len(termArgs))
			}
			result.terminal = pipelineStep{
				name: remaining[0],
				args: termArgs,
			}
			return result, nil
		}

		return nil, fmt.Errorf("unknown pipeline segment: %q", seg)
	}

	// If we consumed everything as modifiers with no terminal
	if result.terminal.name == "" {
		return nil, fmt.Errorf("pipeline has modifiers but no terminal endpoint")
	}

	return result, nil
}

// buildModifierPrefix reconstructs the URL prefix from modifiers.
func buildModifierPrefix(modifiers []pipelineStep) string {
	if len(modifiers) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range modifiers {
		sb.WriteString("/")
		sb.WriteString(m.name)
		for _, a := range m.args {
			sb.WriteString("/")
			sb.WriteString(a)
		}
	}
	return sb.String()
}

// Pipeline is the handler for composable pipeline URLs.
func (h *HTTPBin) Pipeline(w http.ResponseWriter, r *http.Request) {
	result, err := parsePipeline(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Calculate total delay budget (using max of ranges) for delay/response_delay modifiers
	var totalDelay time.Duration
	for _, mod := range result.modifiers {
		if mod.name != "delay" && mod.name != "response_delay" {
			continue
		}
		_, maxD, err := parseDelayRange(mod.args[0], h.MaxDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid %s duration: %w", mod.name, err))
			return
		}
		totalDelay += maxD
	}

	if totalDelay > h.MaxDuration {
		writeError(w, http.StatusBadRequest, fmt.Errorf("total delay %s exceeds maximum %s", totalDelay, h.MaxDuration))
		return
	}

	// Validate header modifiers
	for _, mod := range result.modifiers {
		if mod.name == "header" {
			name, _, found := strings.Cut(mod.args[0], ":")
			if !found || name == "" {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid header format %q: expected name:value", mod.args[0]))
				return
			}
			if !isAllowedMixHeader(name) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("header %q is not allowed", name))
				return
			}
			// Check for CRLF injection
			if strings.ContainsAny(mod.args[0], "\r\n") {
				writeError(w, http.StatusBadRequest, fmt.Errorf("header value contains invalid characters"))
				return
			}
		}
		if mod.name == "status" {
			if _, err := parseStatusCode(mod.args[0]); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid status modifier: %w", err))
				return
			}
		}
	}

	// Apply delays
	var actualDelay time.Duration
	for _, mod := range result.modifiers {
		if mod.name != "delay" && mod.name != "response_delay" {
			continue
		}
		minD, maxD, _ := parseDelayRange(mod.args[0], h.MaxDuration)
		delay := minD
		if maxD > minD {
			delay = minD + time.Duration(rand.Int63n(int64(maxD-minD+1)))
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			actualDelay += delay
		}
	}

	// Add Server-Timing header for the pipeline delay
	if actualDelay > 0 {
		w.Header().Set("Server-Timing", encodeServerTimings([]serverTiming{
			{"pipeline_delay", actualDelay, "pipeline delay"},
		}))
	}

	// Apply header modifiers
	for _, mod := range result.modifiers {
		if mod.name == "header" {
			name, val, _ := strings.Cut(mod.args[0], ":")
			w.Header().Set(name, val)
		}
	}

	// Apply status override wrapper if status modifier is present
	responseWriter := http.ResponseWriter(w)
	for _, mod := range result.modifiers {
		if mod.name == "status" {
			code, _ := parseStatusCode(mod.args[0])
			responseWriter = &statusOverrideResponseWriter{
				ResponseWriter: responseWriter,
				statusOverride: code,
			}
			break // only first status modifier applies
		}
	}

	// Check method restriction (use original writer, not status-overridden)
	def := pipelineTerminals[result.terminal.name]
	if def.method != "" && r.Method != def.method {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed for /%s", r.Method, result.terminal.name))
		return
	}

	modifierPrefix := buildModifierPrefix(result.modifiers)
	h.dispatchTerminal(responseWriter, r, result.terminal, modifierPrefix)
}

// statusOverrideResponseWriter wraps an http.ResponseWriter to override the status code.
type statusOverrideResponseWriter struct {
	http.ResponseWriter
	statusOverride int
	headersDone    bool
}

func (w *statusOverrideResponseWriter) WriteHeader(_ int) {
	if !w.headersDone {
		w.headersDone = true
		w.ResponseWriter.WriteHeader(w.statusOverride)
	}
}

func (w *statusOverrideResponseWriter) Write(b []byte) (int, error) {
	if !w.headersDone {
		w.WriteHeader(w.statusOverride)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusOverrideResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusOverrideResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// dispatchTerminal dispatches to the appropriate handler for a terminal.
func (h *HTTPBin) dispatchTerminal(w http.ResponseWriter, r *http.Request, terminal pipelineStep, modifierPrefix string) {
	def := pipelineTerminals[terminal.name]

	// Set path values for terminals with fixed args
	for i, pvName := range def.pathValues {
		if i < len(terminal.args) {
			r.SetPathValue(pvName, terminal.args[i])
		}
	}

	// Rewrite r.URL.Path to the canonical path for getURL()
	switch terminal.name {
	case "image":
		h.dispatchImageTerminal(w, r, terminal.args)
		return
	case "redirect":
		h.dispatchRedirectTerminal(w, r, "redirect", terminal.args, modifierPrefix, true)
		return
	case "absolute-redirect":
		h.dispatchRedirectTerminal(w, r, "absolute-redirect", terminal.args, modifierPrefix, false)
		return
	case "relative-redirect":
		h.dispatchRedirectTerminal(w, r, "relative-redirect", terminal.args, modifierPrefix, true)
		return
	case "links":
		h.dispatchLinksTerminal(w, r, terminal.args)
		return
	case "cache":
		h.dispatchCacheTerminal(w, r, terminal.args)
		return
	case "base64":
		h.dispatchBase64Terminal(w, r, terminal.args)
		return
	case "digest-auth":
		h.dispatchDigestAuthTerminal(w, r, terminal.args)
		return
	case "body":
		h.dispatchBodyTerminal(w, r, terminal.args)
		return
	}

	// For all other terminals, rewrite path and dispatch
	canonicalPath := "/" + terminal.name
	for _, a := range terminal.args {
		canonicalPath += "/" + a
	}
	r.URL.Path = canonicalPath

	switch terminal.name {
	case "get", "head":
		h.Get(w, r)
	case "post", "put", "delete", "patch":
		h.RequestWithBody(w, r)
	case "status":
		h.Status(w, r)
	case "bytes":
		h.Bytes(w, r)
	case "stream":
		h.Stream(w, r)
	case "stream-bytes":
		h.StreamBytes(w, r)
	case "etag":
		h.ETag(w, r)
	case "range":
		h.Range(w, r)
	case "basic-auth":
		h.BasicAuth(w, r)
	case "hidden-basic-auth":
		h.HiddenBasicAuth(w, r)
	case "gzip":
		h.Gzip(w, r)
	case "deflate":
		h.Deflate(w, r)
	case "html":
		h.HTML(w, r)
	case "json":
		h.JSON(w, r)
	case "xml":
		h.XML(w, r)
	case "robots.txt":
		h.Robots(w, r)
	case "deny":
		h.Deny(w, r)
	case "ip":
		h.IP(w, r)
	case "headers":
		h.Headers(w, r)
	case "user-agent":
		h.UserAgent(w, r)
	case "uuid":
		h.UUID(w, r)
	case "hostname":
		h.Hostname(w, r)
	case "bearer":
		h.Bearer(w, r)
	case "cookies":
		h.Cookies(w, r)
	case "cookies/set":
		h.SetCookies(w, r)
	case "cookies/delete":
		h.DeleteCookies(w, r)
	case "encoding/utf8":
		h.UTF8(w, r)
	case "forms/post":
		h.FormsPost(w, r)
	case "dump/request":
		h.DumpRequest(w, r)
	case "drip":
		h.Drip(w, r)
	case "sse":
		h.SSE(w, r)
	case "unstable":
		h.Unstable(w, r)
	case "anything":
		h.Anything(w, r)
	case "env":
		h.Env(w, r)
	case "pdf":
		h.PDF(w, r)
	case "trailers":
		h.Trailers(w, r)
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown terminal: %s", terminal.name))
	}
}

// imageExtToKind maps file extensions to image kinds.
var imageExtToKind = map[string]string{
	".png":  "png",
	".jpeg": "jpeg",
	".jpg":  "jpeg",
	".svg":  "svg",
	".webp": "webp",
	".avif": "avif",
}

// imagePathTokens defines recognized tokens in image pipeline paths.
// true = consumes 1 value arg, false = flag only (no value).
var imagePathTokens = map[string]bool{
	"size":     true,
	"gradient": true,
	"color1":   true,
	"color2":   true,
	"noise":    true,
	"no-cache": false,
}

// dispatchImageTerminal handles the image terminal with vanity URL support
// and a token consumer loop for path-based parameters.
// Patterns:
//   - /image/png → existing kind
//   - /image/photo.png → kind from extension
//   - /image/size/small/photo.png → generated image with size
//   - /image/gradient/warm/size/medium/photo.png → gradient + size
//   - /image/no-cache/gradient/cool/photo.jpeg → nocache + gradient
func (h *HTTPBin) dispatchImageTerminal(w http.ResponseWriter, r *http.Request, args []string) {
	if len(args) == 0 {
		// /image with no args — use Accept header
		r.URL.Path = "/image"
		h.ImageAccept(w, r)
		return
	}

	// Token consumer loop: consume recognized tokens from args
	q := r.URL.Query()
	i := 0
	for i < len(args) {
		token := args[i]
		takesValue, isToken := imagePathTokens[token]
		if !isToken {
			break // first non-token is filename or kind
		}
		if takesValue {
			if i+1 >= len(args) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("image token %q requires a value", token))
				return
			}
			// Path tokens override query params
			q.Set(token, args[i+1])
			i += 2
		} else {
			// Flag token (no-cache)
			if token == "no-cache" {
				q.Set("nocache", "1")
			}
			i++
		}
	}

	// Update query string with consumed tokens
	r.URL.RawQuery = q.Encode()
	remaining := args[i:]

	if len(remaining) == 0 {
		// All args were tokens, no filename/kind — use Accept header
		r.URL.Path = "/image"
		h.ImageAccept(w, r)
		return
	}

	// Check if remaining[0] has a file extension (vanity URL like photo.png)
	if ext := strings.ToLower(path.Ext(remaining[0])); ext != "" {
		kind, ok := imageExtToKind[ext]
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported image extension: %s", ext))
			return
		}
		r.SetPathValue("kind", kind)
		r.URL.Path = "/image/" + kind
		// If size or gradient params present, use Image handler (dynamic generation)
		if q.Get("size") != "" || q.Get("gradient") != "" || q.Get("color1") != "" {
			h.Image(w, r)
		} else {
			doImage(w, kind)
		}
		return
	}

	// Check if remaining[0] is a known image kind (e.g., /image/png)
	switch remaining[0] {
	case "png", "jpeg", "svg", "webp", "avif":
		if len(remaining) > 1 {
			writeError(w, http.StatusNotFound, nil)
			return
		}
		r.SetPathValue("kind", remaining[0])
		r.URL.Path = "/image/" + remaining[0]
		if q.Get("size") != "" || q.Get("gradient") != "" || q.Get("color1") != "" {
			h.Image(w, r)
		} else {
			doImage(w, remaining[0])
		}
		return
	}

	// Unknown image argument
	writeError(w, http.StatusBadRequest, fmt.Errorf("unknown image parameter: %s", remaining[0]))
}

// dispatchRedirectTerminal handles redirect terminals with pipeline chaining.
func (h *HTTPBin) dispatchRedirectTerminal(w http.ResponseWriter, r *http.Request, redirectType string, args []string, modifierPrefix string, relative bool) {
	if len(args) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("redirect requires a count argument"))
		return
	}

	count, err := strconv.Atoi(args[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid redirect count: %w", err))
		return
	}
	if count < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("redirect count must be > 0"))
		return
	}
	if count > 20 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("redirect count must be <= 20"))
		return
	}

	destSegments := args[1:]

	if count > 1 {
		// Build next redirect URL preserving modifiers and destination
		var nextPath string
		if len(destSegments) > 0 {
			nextPath = fmt.Sprintf("%s/%s/%d/%s", modifierPrefix, redirectType, count-1, strings.Join(destSegments, "/"))
		} else {
			nextPath = fmt.Sprintf("%s/%s/%d", modifierPrefix, redirectType, count-1)
		}
		if relative {
			h.doRedirect(w, nextPath, http.StatusFound)
		} else {
			u := getURL(r)
			u.Path = h.prefix + nextPath
			u.RawQuery = ""
			h.doRedirect(w, u.String(), http.StatusFound)
		}
		return
	}

	// count == 1: redirect to the destination (or /get if none)
	if len(destSegments) > 0 {
		destPath := modifierPrefix + "/" + strings.Join(destSegments, "/")
		if relative {
			h.doRedirect(w, destPath, http.StatusFound)
		} else {
			u := getURL(r)
			u.Path = h.prefix + destPath
			u.RawQuery = ""
			h.doRedirect(w, u.String(), http.StatusFound)
		}
	} else {
		if relative {
			h.doRedirect(w, modifierPrefix+"/get", http.StatusFound)
		} else {
			u := getURL(r)
			u.Path = h.prefix + modifierPrefix + "/get"
			u.RawQuery = ""
			h.doRedirect(w, u.String(), http.StatusFound)
		}
	}
}

// dispatchLinksTerminal handles the /links terminal which takes 1 or 2 args.
func (h *HTTPBin) dispatchLinksTerminal(w http.ResponseWriter, r *http.Request, args []string) {
	if len(args) < 1 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("links requires at least 1 argument"))
		return
	}
	r.SetPathValue("numLinks", args[0])
	offset := "0"
	if len(args) >= 2 {
		offset = args[1]
	}
	r.SetPathValue("offset", offset)
	r.URL.Path = "/links/" + args[0] + "/" + offset
	h.Links(w, r)
}

// dispatchCacheTerminal handles the /cache terminal which takes 0 or 1 args.
func (h *HTTPBin) dispatchCacheTerminal(w http.ResponseWriter, r *http.Request, args []string) {
	if len(args) == 0 {
		r.URL.Path = "/cache"
		h.Cache(w, r)
		return
	}
	r.SetPathValue("numSeconds", args[0])
	r.URL.Path = "/cache/" + args[0]
	h.CacheControl(w, r)
}

// dispatchBase64Terminal handles the /base64 terminal which takes 1 or 2 args.
func (h *HTTPBin) dispatchBase64Terminal(w http.ResponseWriter, r *http.Request, args []string) {
	if len(args) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("base64 requires at least 1 argument"))
		return
	}
	if len(args) == 1 {
		r.SetPathValue("data", args[0])
		r.URL.Path = "/base64/" + args[0]
	} else {
		r.SetPathValue("operation", args[0])
		r.SetPathValue("data", args[1])
		r.URL.Path = "/base64/" + args[0] + "/" + args[1]
	}
	h.Base64(w, r)
}

// dispatchDigestAuthTerminal handles /digest-auth which takes 3 or 4 args.
func (h *HTTPBin) dispatchDigestAuthTerminal(w http.ResponseWriter, r *http.Request, args []string) {
	if len(args) < 3 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("digest-auth requires at least 3 arguments: qop, user, password"))
		return
	}
	r.SetPathValue("qop", args[0])
	r.SetPathValue("user", args[1])
	r.SetPathValue("password", args[2])
	if len(args) >= 4 {
		r.SetPathValue("algorithm", args[3])
		r.URL.Path = "/digest-auth/" + strings.Join(args[:4], "/")
	} else {
		r.URL.Path = "/digest-auth/" + strings.Join(args[:3], "/")
	}
	h.DigestAuth(w, r)
}

// dispatchBodyTerminal handles the /body terminal which returns base64-decoded content.
func (h *HTTPBin) dispatchBodyTerminal(w http.ResponseWriter, _ *http.Request, args []string) {
	if len(args) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body terminal requires a base64-encoded argument"))
		return
	}

	data := args[0]

	// Try URL-safe base64 first, then standard
	decoded, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			decoded, err = base64.StdEncoding.DecodeString(data)
			if err != nil {
				decoded, err = base64.RawStdEncoding.DecodeString(data)
				if err != nil {
					writeError(w, http.StatusBadRequest, fmt.Errorf("invalid base64 data: %w", err))
					return
				}
			}
		}
	}

	w.Header().Set("Content-Type", textContentType)
	w.Write(decoded)
}
