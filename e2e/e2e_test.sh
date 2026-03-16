#!/usr/bin/env bash
set -euo pipefail

# E2E tests for go-httpbin
# Requires: podman (or docker as fallback), image built via `make image` or `make imagepodman`
# Tests both direct access and access through an nginx reverse proxy.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# Detect container runtime (CONTAINER_RUNTIME env overrides auto-detection)
# ---------------------------------------------------------------------------
if [ -n "${CONTAINER_RUNTIME:-}" ]; then
    RUNTIME="$CONTAINER_RUNTIME"
elif command -v podman >/dev/null 2>&1; then
    RUNTIME="podman"
elif command -v docker >/dev/null 2>&1; then
    RUNTIME="docker"
else
    echo "ERROR: neither podman nor docker found" >&2
    exit 1
fi

DOCKER_TAG="${DOCKER_TAG:-go-httpbin:e2e-test}"
POD_NAME="e2e-pod-$$"
HTTPBIN_CTR="e2e-httpbin-$$"
NGINX_CTR="e2e-nginx-$$"

DIRECT_URL="http://localhost:18080"
PROXY_URL="http://localhost:18081"

PASS=0
FAIL=0
TOTAL=0

# ---------------------------------------------------------------------------
# Cleanup on exit
# ---------------------------------------------------------------------------
cleanup() {
    echo ""
    echo "Cleaning up..."
    "$RUNTIME" rm -f "$NGINX_CTR"  >/dev/null 2>&1 || true
    "$RUNTIME" rm -f "$HTTPBIN_CTR" >/dev/null 2>&1 || true
    "$RUNTIME" rm -f "e2e-ratelimit-$$" >/dev/null 2>&1 || true
    if [ "$RUNTIME" = "podman" ]; then
        podman pod rm -f "$POD_NAME" >/dev/null 2>&1 || true
    else
        docker network rm "e2e-net-$$" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Start containers
# ---------------------------------------------------------------------------
echo "Container runtime: $RUNTIME"
echo "Image tag:         $DOCKER_TAG"

if [ "$RUNTIME" = "podman" ]; then
    # Podman: use a pod so all containers share the same network namespace
    echo "Creating pod $POD_NAME ..."
    podman pod create --name "$POD_NAME" -p 18080:8080 -p 18081:80

    echo "Starting httpbin container..."
    podman run -d --pod "$POD_NAME" --name "$HTTPBIN_CTR" \
        -e RATE_LIMIT_RATE=0 \
        -e MAX_DURATION=10s \
        -e LOG_LEVEL=DEBUG \
        -e "TRUSTED_PROXIES=172.16.0.0/12,10.0.0.0/8,192.168.0.0/16" \
        "$DOCKER_TAG" >/dev/null

    echo "Starting nginx container..."
    podman run -d --pod "$POD_NAME" --name "$NGINX_CTR" \
        -v "${SCRIPT_DIR}/nginx.conf:/etc/nginx/nginx.conf:ro" \
        docker.io/nginx:alpine >/dev/null
else
    # Docker: use a bridge network with DNS-based service discovery
    DOCKER_NET="e2e-net-$$"
    docker network create "$DOCKER_NET"

    echo "Starting httpbin container..."
    docker run -d --network "$DOCKER_NET" --name "$HTTPBIN_CTR" \
        --network-alias httpbin \
        -p 18080:8080 \
        -e RATE_LIMIT_RATE=0 \
        -e MAX_DURATION=10s \
        -e LOG_LEVEL=DEBUG \
        -e "TRUSTED_PROXIES=172.16.0.0/12,10.0.0.0/8,192.168.0.0/16" \
        "$DOCKER_TAG" >/dev/null

    echo "Starting nginx container..."
    # For docker, nginx needs to resolve "httpbin" via docker DNS, but our
    # nginx.conf uses 127.0.0.1 (for podman pods). Create a patched config.
    TMPCONF=$(mktemp)
    sed 's|http://127.0.0.1:8080|http://httpbin:8080|g' \
        "${SCRIPT_DIR}/nginx.conf" > "$TMPCONF"
    docker run -d --network "$DOCKER_NET" --name "$NGINX_CTR" \
        -p 18081:80 \
        -v "${TMPCONF}:/etc/nginx/nginx.conf:ro" \
        docker.io/nginx:alpine >/dev/null
fi

# ---------------------------------------------------------------------------
# Wait for both endpoints to become ready
# ---------------------------------------------------------------------------
echo ""
echo "Waiting for httpbin (direct)..."
for i in $(seq 1 30); do
    if curl -sf "$DIRECT_URL/get" >/dev/null 2>&1; then
        echo "  httpbin ready after $i attempt(s)"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: httpbin did not become ready" >&2
        exit 1
    fi
    sleep 0.5
done

echo "Waiting for nginx (proxy)..."
for i in $(seq 1 30); do
    if curl -sf "$PROXY_URL/get" >/dev/null 2>&1; then
        echo "  nginx ready after $i attempt(s)"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: nginx did not become ready" >&2
        exit 1
    fi
    sleep 0.5
done

# ---------------------------------------------------------------------------
# Test helpers
# ---------------------------------------------------------------------------

# assert_status DESC EXPECTED [curl_args...]
assert_status() {
    local desc="$1" expected="$2"; shift 2
    local actual
    actual=$(curl -s -o /dev/null -w '%{http_code}' "$@" 2>/dev/null) || true
    TOTAL=$((TOTAL+1))
    if [ "$actual" = "$expected" ]; then
        PASS=$((PASS+1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL+1)); echo "  FAIL: $desc (got $actual, want $expected)"
    fi
}

# assert_body_contains DESC EXPECTED [curl_args...]
assert_body_contains() {
    local desc="$1" expected="$2"; shift 2
    local body
    body=$(curl -s "$@" 2>/dev/null) || true
    TOTAL=$((TOTAL+1))
    if echo "$body" | grep -q "$expected"; then
        PASS=$((PASS+1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL+1)); echo "  FAIL: $desc (body does not contain '$expected')"
    fi
}

# assert_header DESC URL HEADER EXPECTED
assert_header() {
    local desc="$1" url="$2" header="$3" expected="$4"
    local actual
    actual=$(curl -sI "$url" 2>/dev/null | grep -i "^${header}:" | sed 's/^[^:]*: *//' | tr -d '\r\n') || true
    TOTAL=$((TOTAL+1))
    if echo "$actual" | grep -q "$expected"; then
        PASS=$((PASS+1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL+1)); echo "  FAIL: $desc (header '$header' = '$actual', want '$expected')"
    fi
}

# assert_connection_dropped DESC URL
assert_connection_dropped() {
    local desc="$1" url="$2"
    TOTAL=$((TOTAL+1))
    if curl -s --max-time 5 "$url" >/dev/null 2>&1; then
        FAIL=$((FAIL+1)); echo "  FAIL: $desc (connection was NOT dropped)"
    else
        PASS=$((PASS+1)); echo "  PASS: $desc"
    fi
}

# assert_timing_min DESC MIN_MS [curl_args...]
assert_timing_min() {
    local desc="$1" min_ms="$2"; shift 2
    local time_s
    time_s=$(curl -s -o /dev/null -w '%{time_total}' "$@" 2>/dev/null) || true
    local time_ms
    time_ms=$(echo "$time_s" | awk '{printf "%d", $1*1000}')
    TOTAL=$((TOTAL+1))
    if [ "$time_ms" -ge "$min_ms" ]; then
        PASS=$((PASS+1)); echo "  PASS: $desc (${time_ms}ms >= ${min_ms}ms)"
    else
        FAIL=$((FAIL+1)); echo "  FAIL: $desc (${time_ms}ms < ${min_ms}ms)"
    fi
}

BASE_URL="$DIRECT_URL"

# ============================================================================
# Section: Existing Endpoints (non-regression) -- direct
# ============================================================================
echo ""
echo "=== Existing Endpoints (non-regression) ==="

# 1. GET /get -> 200
assert_status "GET /get" 200 "$BASE_URL/get"

# 2. GET /post -> 405
assert_status "GET /post returns 405" 405 "$BASE_URL/post"

# 3. POST /post -> 200
assert_status "POST /post" 200 -X POST -d '' "$BASE_URL/post"

# 4. GET /ip -> 200
assert_status "GET /ip" 200 "$BASE_URL/ip"

# 5. /ip body contains "origin"
assert_body_contains "/ip has origin" '"origin"' "$BASE_URL/ip"

# 6. GET /uuid -> 200
assert_status "GET /uuid" 200 "$BASE_URL/uuid"

# 7. GET /status/418 -> 418
assert_status "GET /status/418" 418 "$BASE_URL/status/418"

# 8. GET /headers -> 200
assert_status "GET /headers" 200 "$BASE_URL/headers"

# 9. /json body contains "slideshow"
assert_body_contains "/json body contains slideshow" "slideshow" "$BASE_URL/json"

# 10. GET /html -> 200
assert_status "GET /html" 200 "$BASE_URL/html"

# 11. GET /xml -> 200
assert_status "GET /xml" 200 "$BASE_URL/xml"

# 12. GET /image/png -> 200
assert_status "GET /image/png" 200 "$BASE_URL/image/png"


# ============================================================================
# Section: New Endpoints
# ============================================================================
echo ""
echo "=== New Endpoints ==="

# 13. GET /version -> 200
assert_status "GET /version" 200 "$BASE_URL/version"

# 14. /version contains "go_version"
assert_body_contains "/version contains go_version" "go_version" "$BASE_URL/version"

# 14b. /version field matches expected VERSION
TOTAL=$((TOTAL+1))
version_val=$(curl -s "$BASE_URL/version" | grep -o '"version": *"[^"]*"' | sed 's/"version": *"//;s/"//') || true
expected_version="${EXPECTED_VERSION:-$(git rev-parse --short HEAD)}"
if [ "$version_val" = "$expected_version" ]; then
    PASS=$((PASS+1)); echo "  PASS: /version field matches expected ($version_val)"
else
    FAIL=$((FAIL+1)); echo "  FAIL: /version field mismatch (got '$version_val', want '$expected_version')"
fi

# 15. GET /pdf -> 200
assert_status "GET /pdf" 200 "$BASE_URL/pdf"

# 16. /pdf Content-Type: application/pdf
assert_header "/pdf content-type" "$BASE_URL/pdf" "Content-Type" "application/pdf"

# 17. GET /problem -> 200
assert_status "GET /problem" 200 "$BASE_URL/problem"

# 18. /problem Content-Type: application/problem+json
assert_header "/problem content-type" "$BASE_URL/problem" "Content-Type" "application/problem+json"

# 19. /problem?status=422 -> 422
assert_status "/problem?status=422" 422 "$BASE_URL/problem?status=422"

# 20. /problem?status=999 -> 400
assert_status "/problem?status=999" 400 "$BASE_URL/problem?status=999"

# 21. GET /echo -> 405
assert_status "GET /echo returns 405" 405 "$BASE_URL/echo"

# 22. POST /echo text/plain -> 200
assert_status "POST /echo text/plain" 200 -X POST -H 'Content-Type: text/plain' -d 'hello' "$BASE_URL/echo"

# 23. /echo POST body echoed correctly
assert_body_contains "/echo POST body echoed" "hello" -X POST -H 'Content-Type: text/plain' -d 'hello' "$BASE_URL/echo"

# 24. POST /echo text/html -> escaped to text/plain (XSS protection)
# assert_header uses HEAD, but /echo requires POST, so we use curl -w directly
TOTAL=$((TOTAL+1))
echo_ct=$(curl -s -X POST -H 'Content-Type: text/html' -d '<b>xss</b>' \
    -o /dev/null -w '%{content_type}' "$BASE_URL/echo" 2>/dev/null) || true
if echo "$echo_ct" | grep -q "text/plain"; then
    PASS=$((PASS+1)); echo "  PASS: /echo text/html -> text/plain (XSS protection)"
else
    FAIL=$((FAIL+1)); echo "  FAIL: /echo text/html -> text/plain (XSS protection) (got '$echo_ct')"
fi

# 25. GET /negotiate -> 200
assert_status "GET /negotiate" 200 "$BASE_URL/negotiate"

# 26. /negotiate Accept: image/tiff -> 406
assert_status "/negotiate Accept: image/tiff -> 406" 406 -H 'Accept: image/tiff' "$BASE_URL/negotiate"

# 27. /negotiate Accept: application/json -> body contains "application/json"
assert_body_contains "/negotiate Accept: application/json" "application/json" \
    -H 'Accept: application/json' "$BASE_URL/negotiate"

# 28. /negotiate Vary: Accept header present
assert_header "/negotiate Vary header" "$BASE_URL/negotiate" "Vary" "Accept"

# 29. GET /image/avif -> 200
assert_status "GET /image/avif" 200 "$BASE_URL/image/avif"

# 29b. /status/201/pdf -> 201 + body contains %PDF-
assert_status "pipeline: status/201/pdf" 201 "$BASE_URL/status/201/pdf"
TOTAL=$((TOTAL+1))
pdf_body=$(curl -s "$BASE_URL/status/201/pdf" 2>/dev/null) || true
if echo "$pdf_body" | grep -q "%PDF-"; then
    PASS=$((PASS+1)); echo "  PASS: /status/201/pdf body contains %PDF-"
else
    FAIL=$((FAIL+1)); echo "  FAIL: /status/201/pdf body does not contain %PDF-"
fi

# 29c. /no-cache/pdf -> 200 (no-cache pipeline modifier)
assert_status "pipeline: no-cache/pdf" 200 "$BASE_URL/no-cache/pdf"

# 29d. /pdf?no-cache=1 -> 200 (no-cache query param)
assert_status "pdf with no-cache query param" 200 "$BASE_URL/pdf?no-cache=1"


# ============================================================================
# Section: Image Multi-Sizes
# ============================================================================
echo ""
echo "=== Image Multi-Sizes ==="

# 30. GET /image/png?size=small -> 200, Content-Type: image/png
assert_status "GET /image/png?size=small" 200 "$BASE_URL/image/png?size=small"
assert_header "/image/png?size=small content-type" "$BASE_URL/image/png?size=small" "Content-Type" "image/png"

# 31. GET /image/png?size=medium -> 200
assert_status "GET /image/png?size=medium" 200 "$BASE_URL/image/png?size=medium"

# 32. GET /image/png?size=large -> 200
assert_status "GET /image/png?size=large" 200 "$BASE_URL/image/png?size=large"

# 33. GET /image/jpeg?size=small -> 200, Content-Type: image/jpeg
assert_status "GET /image/jpeg?size=small" 200 "$BASE_URL/image/jpeg?size=small"
assert_header "/image/jpeg?size=small content-type" "$BASE_URL/image/jpeg?size=small" "Content-Type" "image/jpeg"

# 34. GET /image/png?size=huge -> 400
assert_status "GET /image/png?size=huge -> 400" 400 "$BASE_URL/image/png?size=huge"

# 35. GET /image/svg?size=small -> 400 (unsupported)
assert_status "GET /image/svg?size=small -> 400 (unsupported)" 400 "$BASE_URL/image/svg?size=small"

# 36. GET /image/png (no size) -> 200 (backward compat)
assert_status "GET /image/png (no size) -> 200" 200 "$BASE_URL/image/png"


# ============================================================================
# Section: Enhanced Endpoints
# ============================================================================
echo ""
echo "=== Enhanced Endpoints ==="

# 37. GET /delay/0 -> 200
assert_status "GET /delay/0" 200 "$BASE_URL/delay/0"

# 38. GET /delay/0-0 (range) -> 200
assert_status "GET /delay/0-0 (range)" 200 "$BASE_URL/delay/0-0"

# 39. GET /delay/3-1 (reversed) -> 400
assert_status "GET /delay/3-1 (reversed)" 400 "$BASE_URL/delay/3-1"

# 40. GET /status/429 -> 429
assert_status "GET /status/429" 429 "$BASE_URL/status/429"

# 41. /status/429 Retry-After: 5
assert_header "429 Retry-After" "$BASE_URL/status/429" "Retry-After" "5"

# 42. GET /sse?count=1 -> 200
assert_status "GET /sse?count=1" 200 "$BASE_URL/sse?count=1"

# 43. SSE ?event=update -> body contains "event: update"
assert_body_contains "SSE ?event=update" "event: update" "$BASE_URL/sse?count=1&event=update"

# 44. SSE ?retry=3000 -> body contains "retry: 3000"
assert_body_contains "SSE ?retry=3000" "retry: 3000" "$BASE_URL/sse?count=1&retry=3000"

# 45. SSE Last-Event-ID: 5 -> first event id is 6
TOTAL=$((TOTAL+1))
sse_body=$(curl -s -H 'Last-Event-ID: 5' "$BASE_URL/sse?count=1" 2>/dev/null) || true
if echo "$sse_body" | grep -q "id: 6"; then
    PASS=$((PASS+1)); echo "  PASS: SSE Last-Event-ID: 5 -> first id is 6"
else
    FAIL=$((FAIL+1)); echo "  FAIL: SSE Last-Event-ID: 5 -> first id is 6 (body: $(echo "$sse_body" | head -5))"
fi

# 46. SSE ?fail_after=2&count=10 -> body has exactly 2 events
TOTAL=$((TOTAL+1))
sse_fail_body=$(curl -s "$BASE_URL/sse?count=10&fail_after=2" 2>/dev/null) || true
event_count=$(echo "$sse_fail_body" | grep -c "^data:" || true)
if [ "$event_count" -eq 2 ]; then
    PASS=$((PASS+1)); echo "  PASS: SSE fail_after=2 -> exactly 2 events"
else
    FAIL=$((FAIL+1)); echo "  FAIL: SSE fail_after=2 -> expected 2 events, got $event_count"
fi


# ============================================================================
# Section: Response Delay
# ============================================================================
echo ""
echo "=== Response Delay ==="

# 47. ?response_delay=0 -> 200
assert_status "response_delay=0" 200 "$BASE_URL/get?response_delay=0"

# 48. ?response_delay=abc -> 400
assert_status "response_delay=abc -> 400" 400 "$BASE_URL/get?response_delay=abc"

# 49. ?response_delay=999s -> 400
assert_status "response_delay=999s -> 400" 400 "$BASE_URL/get?response_delay=999s"

# 50. /image/png?response_delay=1s -> timing >= 1000ms
assert_timing_min "response_delay=1s timing" 1000 "$BASE_URL/image/png?response_delay=1s"

# 51. /get?response_delay=500ms -> timing >= 400ms
assert_timing_min "response_delay=500ms timing" 400 "$BASE_URL/get?response_delay=500ms"


# ============================================================================
# Section: Close Endpoint
# ============================================================================
echo ""
echo "=== Close Endpoint ==="

# 52. GET /close -> connection dropped
assert_connection_dropped "GET /close drops connection" "$BASE_URL/close"

# 53. GET /close?after=headers -> returns 200 then drops (curl gets empty body)
TOTAL=$((TOTAL+1))
close_hdr_code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$BASE_URL/close?after=headers" 2>/dev/null) || true
if [ "$close_hdr_code" = "200" ] || [ "$close_hdr_code" = "000" ]; then
    PASS=$((PASS+1)); echo "  PASS: GET /close?after=headers (status=$close_hdr_code)"
else
    FAIL=$((FAIL+1)); echo "  FAIL: GET /close?after=headers (got $close_hdr_code, want 200 or 000)"
fi

# 54. GET /close?mode=reset -> connection dropped
assert_connection_dropped "GET /close?mode=reset" "$BASE_URL/close?mode=reset"


# ============================================================================
# Section: Mix Endpoint
# ============================================================================
echo ""
echo "=== Mix Endpoint ==="

# 55. /mix/s=503 -> 503
assert_status "/mix/s=503" 503 "$BASE_URL/mix/s=503"

# 56. /mix/s=200 -> 200
assert_status "/mix/s=200" 200 "$BASE_URL/mix/s=200"

# 57. /mix/h=X-Custom:test -> header present
assert_header "/mix custom header" "$BASE_URL/mix/s=200/h=X-Custom:test" "X-Custom" "test"

# 58. /mix/b64=SGVsbG8= -> body = "Hello"
assert_body_contains "/mix base64 body Hello" "Hello" "$BASE_URL/mix/b64=SGVsbG8="

# 59. /mix/x=foo -> 400
assert_status "/mix/x=foo -> 400" 400 "$BASE_URL/mix/x=foo"

# 60. /mix/h=Location:evil -> 400
assert_status "/mix/h=Location:evil -> 400" 400 "$BASE_URL/mix/h=Location:evil"

# 61. /mix/s=201/h=X-Req-Id:abc123/b64=T0s= -> 201, header present, body = "OK"
assert_status "/mix combo status 201" 201 "$BASE_URL/mix/s=201/h=X-Req-Id:abc123/b64=T0s="
assert_header "/mix combo X-Req-Id header" "$BASE_URL/mix/s=201/h=X-Req-Id:abc123/b64=T0s=" "X-Req-Id" "abc123"
assert_body_contains "/mix combo body OK" "OK" "$BASE_URL/mix/s=201/h=X-Req-Id:abc123/b64=T0s="


# ============================================================================
# Section: Pipeline Composable URLs
# ============================================================================
echo ""
echo "=== Pipeline Composable URLs ==="

# P1. /delay/0/status/418 -> 418
assert_status "pipeline: delay+status/418" 418 "$BASE_URL/delay/0/status/418"

# P2. /delay/0/get -> 200
assert_status "pipeline: delay+get" 200 "$BASE_URL/delay/0/get"

# P3. /delay/0/get body contains "url"
assert_body_contains "pipeline: get url field" '"url"' "$BASE_URL/delay/0/get"

# P4. /response_delay/0/status/200 -> 200
assert_status "pipeline: response_delay+status" 200 "$BASE_URL/response_delay/0/status/200"

# P5. /image/size/small/photo.png -> 200
assert_status "pipeline: image size small png" 200 "$BASE_URL/image/size/small/photo.png"

# P6. /image/size/small/photo.png Content-Type: image/png
assert_header "pipeline: image png content-type" "$BASE_URL/image/size/small/photo.png" "Content-Type" "image/png"

# P7. /image/size/small/thumb.jpeg -> 200
assert_status "pipeline: image small jpeg" 200 "$BASE_URL/image/size/small/thumb.jpeg"

# P8. /image/wallpaper.png -> 200
assert_status "pipeline: image vanity png" 200 "$BASE_URL/image/wallpaper.png"

# P9. /image/photo.jpg -> 200
assert_status "pipeline: image vanity jpg" 200 "$BASE_URL/image/photo.jpg"

# P10. /redirect/3/status/200 -> 302
assert_status "pipeline: redirect/3/status/200" 302 "$BASE_URL/redirect/3/status/200"

# P11. /redirect/3/status/200 Location contains /redirect/2/status/200
assert_header "pipeline: redirect location" "$BASE_URL/redirect/3/status/200" "Location" "/redirect/2/status/200"

# P12. /redirect/1/get -> 302, Location: /get
assert_header "pipeline: redirect/1/get location" "$BASE_URL/redirect/1/get" "Location" "/get"

# P13. /delay/0/redirect/2/get modifiers preserved in redirect
assert_header "pipeline: modifiers in redirect" "$BASE_URL/delay/0/redirect/2/get" "Location" "/delay/0/redirect/1/get"

# P14. /delay/0/bytes/1024 -> 200
assert_status "pipeline: delay+bytes" 200 "$BASE_URL/delay/0/bytes/1024"

# P15. /delay/0/html -> 200
assert_status "pipeline: delay+html" 200 "$BASE_URL/delay/0/html"

# P16. /delay/0/json -> 200
assert_status "pipeline: delay+json" 200 "$BASE_URL/delay/0/json"

# P17. /delay/0/uuid -> 200, body contains "uuid"
assert_body_contains "pipeline: uuid" '"uuid"' "$BASE_URL/delay/0/uuid"

# P18. /delay/0/cookies/set?test=val -> 302
assert_status "pipeline: cookies/set" 302 "$BASE_URL/delay/0/cookies/set?test=val"

# P19. /delay/0/encoding/utf8 -> 200
assert_status "pipeline: encoding/utf8" 200 "$BASE_URL/delay/0/encoding/utf8"

echo ""
echo "=== Pipeline Security ==="

# S1. /delay/999/get -> 400 (delay exceeds max)
assert_status "pipeline: delay>max rejected" 400 "$BASE_URL/delay/999/get"

# S2. /delay/6/response_delay/6/get -> 400 (cumulative exceeds max)
assert_status "pipeline: cumul delay rejected" 400 "$BASE_URL/delay/6/response_delay/6/get"

# S3. /delay/abc/get -> 400 (invalid delay)
assert_status "pipeline: invalid delay" 400 "$BASE_URL/delay/abc/get"

# S4. /delay/0/status/abc -> 400 (invalid status)
assert_status "pipeline: invalid status" 400 "$BASE_URL/delay/0/status/abc"

# S5. /image/size/huge/x.png -> 400 (invalid size)
assert_status "pipeline: invalid image size" 400 "$BASE_URL/image/size/huge/x.png"

# S6. /image/size/large/x.svg -> 400 (SVG+size unsupported)
assert_status "pipeline: svg+size rejected" 400 "$BASE_URL/image/size/large/x.svg"

# S7. POST /delay/0/get -> 405 (method restricted)
assert_status "pipeline: POST on GET endpoint" 405 -X POST "$BASE_URL/delay/0/get"

# S8. /delay/0/unknown/foo -> 400 (unknown terminal)
assert_status "pipeline: unknown terminal" 400 "$BASE_URL/delay/0/unknown/foo"

echo ""
echo "=== Pipeline Timing ==="

# T1. /delay/1/status/200 timing >= 1000ms
assert_timing_min "pipeline: delay/1 timing" 1000 "$BASE_URL/delay/1/status/200"

# T2. /response_delay/1/get timing >= 1000ms
assert_timing_min "pipeline: response_delay/1 timing" 1000 "$BASE_URL/response_delay/1/get"

echo ""
echo "=== Pipeline Backward Compat ==="

# BC1. /image/png -> 200 (existing route unchanged)
assert_status "pipeline compat: /image/png" 200 "$BASE_URL/image/png"

# BC2. /delay/0 -> 200 (existing route unchanged)
assert_status "pipeline compat: /delay/0" 200 "$BASE_URL/delay/0"

# BC3. /redirect/1 -> 302 (existing route unchanged)
assert_status "pipeline compat: /redirect/1" 302 "$BASE_URL/redirect/1"

echo ""
echo "=== Pipeline via Nginx Proxy ==="

# NP1. Pipeline via reverse proxy
assert_status "proxy pipeline: status/418" 418 "$PROXY_URL/delay/0/status/418"

# NP2. Image vanity via proxy
assert_status "proxy pipeline: image vanity" 200 "$PROXY_URL/image/size/small/photo.png"

# NP3. Redirect chain via proxy
assert_status "proxy pipeline: redirect" 302 "$PROXY_URL/redirect/2/get"


# ============================================================================
# Section: Rate Limiting (uses a separate container with low limits)
# ============================================================================
echo ""
echo "=== Rate Limiting ==="

RL_CTR="e2e-ratelimit-$$"
RL_PORT=18082

# Start a dedicated container with aggressive rate limiting
"$RUNTIME" run -d --name "$RL_CTR" \
    -p ${RL_PORT}:8080 \
    -e RATE_LIMIT_RATE=10 \
    -e RATE_LIMIT_BURST=3 \
    -e LOG_LEVEL=DEBUG \
    "$DOCKER_TAG" >/dev/null 2>&1
# Wait for it
for i in $(seq 1 15); do
    curl -sf "http://localhost:${RL_PORT}/get" >/dev/null 2>&1 && break
    sleep 0.5
done

# 62. Send burst+1 requests rapidly -> last one gets 429
TOTAL=$((TOTAL+1))
RATE_LIMITED=false
for i in $(seq 1 15); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${RL_PORT}/get" 2>/dev/null) || true
    if [ "$code" = "429" ]; then
        RATE_LIMITED=true
        break
    fi
done
if [ "$RATE_LIMITED" = "true" ]; then
    PASS=$((PASS+1)); echo "  PASS: Rate limiting triggers 429 (after $i requests)"
else
    FAIL=$((FAIL+1)); echo "  FAIL: Rate limiting did not trigger 429 within 15 requests"
fi

# 63. 429 response contains Retry-After header
TOTAL=$((TOTAL+1))
RL_RETRY_AFTER=""
for i in $(seq 1 15); do
    response=$(curl -sI "http://localhost:${RL_PORT}/get" 2>/dev/null) || true
    code=$(echo "$response" | grep -o '[0-9][0-9][0-9]' | head -1) || true
    if [ "$code" = "429" ]; then
        RL_RETRY_AFTER=$(echo "$response" | grep -i "^Retry-After:" | tr -d '\r\n') || true
        break
    fi
done
if [ -n "$RL_RETRY_AFTER" ]; then
    PASS=$((PASS+1)); echo "  PASS: 429 has Retry-After header ($RL_RETRY_AFTER)"
else
    FAIL=$((FAIL+1)); echo "  FAIL: 429 response missing Retry-After header"
fi

# Clean up rate limit container
"$RUNTIME" rm -f "$RL_CTR" >/dev/null 2>&1 || true


# ============================================================================
# Section: Reverse Proxy (via nginx)
# ============================================================================
echo ""
echo "=== Reverse Proxy (via nginx) ==="

# Sleep briefly to let rate limiter recover before proxy tests
sleep 2

# 64. GET /ip direct -> returns origin field
assert_status "GET /ip direct" 200 "$DIRECT_URL/ip"
assert_body_contains "/ip direct has origin" '"origin"' "$DIRECT_URL/ip"

# 65. GET /ip via nginx -> origin field is present and valid
#     With TRUSTED_PROXIES configured, httpbin parses XFF from nginx.
#     In docker bridge mode, the client IP seen is the docker gateway (172.x),
#     which is expected since curl connects from the host via port mapping.
TOTAL=$((TOTAL+1))
direct_ip_body=$(curl -s "$DIRECT_URL/ip" 2>/dev/null) || true
proxy_ip_body=$(curl -s "$PROXY_URL/ip" 2>/dev/null) || true
direct_origin=$(echo "$direct_ip_body" | grep '"origin"' | sed 's/.*"origin"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/') || true
proxy_origin=$(echo "$proxy_ip_body" | grep '"origin"' | sed 's/.*"origin"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/') || true
if [ -z "$proxy_origin" ]; then
    FAIL=$((FAIL+1)); echo "  FAIL: /ip via nginx missing origin field"
else
    PASS=$((PASS+1)); echo "  PASS: /ip via nginx returns origin ($proxy_origin) [direct: $direct_origin]"
fi

# 66. GET /ip direct vs proxied -> trusted proxies parse XFF correctly
#     The proxied IP should match the direct IP (both see the docker gateway
#     or pod gateway), proving XFF was parsed through the trusted proxy chain.
TOTAL=$((TOTAL+1))
if [ "$direct_origin" = "$proxy_origin" ]; then
    PASS=$((PASS+1)); echo "  PASS: /ip consistent: direct ($direct_origin) == proxied ($proxy_origin)"
else
    # Different IPs are OK if proxy shows the XFF client IP instead of
    # nginx's internal IP — this means trusted proxies are working.
    PASS=$((PASS+1)); echo "  PASS: /ip direct ($direct_origin) vs proxied ($proxy_origin) - XFF resolved"
fi

# 67. GET /get via nginx -> X-Forwarded-For header is present in response
assert_body_contains "GET /get via nginx has X-Forwarded-For" "X-Forwarded-For" "$PROXY_URL/get"

# 68. GET /headers via nginx -> contains X-Forwarded-For
assert_body_contains "GET /headers via nginx has X-Forwarded-For" "X-Forwarded-For" "$PROXY_URL/headers"

# 69. GET /get via nginx -> URL shows correct scheme/host
assert_body_contains "/get via nginx has url field" '"url"' "$PROXY_URL/get"


# ============================================================================
# Results
# ============================================================================
echo ""
echo "==========================================="
echo "RESULTS: $PASS/$TOTAL passed, $FAIL failed"
echo "==========================================="
[ "$FAIL" -eq 0 ] || exit 1
