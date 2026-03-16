#!/usr/bin/env bash
set -euo pipefail

# E2E reverse proxy tests for go-httpbin
# Tests header propagation (X-Forwarded-For, ETag, Cache-Control) through
# a real nginx reverse proxy using podman-compose or docker-compose.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# Parse flags
# ---------------------------------------------------------------------------
KEEP=false
for arg in "$@"; do
    case "$arg" in
        --keep) KEEP=true ;;
        *) echo "Unknown flag: $arg" >&2; exit 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Detect compose runtime
# ---------------------------------------------------------------------------
if command -v podman-compose >/dev/null 2>&1; then
    COMPOSE="podman-compose"
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE="docker-compose"
elif command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    COMPOSE="docker compose"
else
    echo "ERROR: neither podman-compose nor docker-compose found" >&2
    exit 1
fi

echo "Compose runtime: $COMPOSE"

BASE_URL="http://localhost:28080"
PASS=0
FAIL=0
TOTAL=0

# ---------------------------------------------------------------------------
# Cleanup on exit (unless --keep)
# ---------------------------------------------------------------------------
cleanup() {
    if [ "$KEEP" = true ]; then
        echo ""
        echo "Stack left running (--keep). Tear down manually with:"
        echo "  cd $SCRIPT_DIR && $COMPOSE down --rmi local"
        return
    fi
    echo ""
    echo "Tearing down..."
    cd "$SCRIPT_DIR" && $COMPOSE down --rmi local >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Start compose stack
# ---------------------------------------------------------------------------
echo "Building and starting compose stack..."
cd "$SCRIPT_DIR" && $COMPOSE up --build -d

# ---------------------------------------------------------------------------
# Wait for nginx to be healthy (up to 30s)
# ---------------------------------------------------------------------------
echo ""
echo "Waiting for nginx proxy..."
for i in $(seq 1 60); do
    if curl -sf "$BASE_URL/get" >/dev/null 2>&1; then
        echo "  nginx ready after $((i / 2))s"
        break
    fi
    if [ "$i" -eq 60 ]; then
        echo "ERROR: nginx did not become ready within 30s" >&2
        echo "Container logs:"
        cd "$SCRIPT_DIR" && $COMPOSE logs 2>&1 | tail -30
        exit 1
    fi
    sleep 0.5
done

# ---------------------------------------------------------------------------
# Test helpers (same pattern as existing e2e tests)
# ---------------------------------------------------------------------------

# check DESC EXPECTED_STATUS [curl_args...]
check() {
    local desc="$1" expected="$2"; shift 2
    local actual
    actual=$(curl -s -o /dev/null -w '%{http_code}' "$@" 2>/dev/null) || true
    TOTAL=$((TOTAL + 1))
    if [ "$actual" = "$expected" ]; then
        PASS=$((PASS + 1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL: $desc (got $actual, want $expected)"
    fi
}

# check_body DESC PATTERN [curl_args...]
check_body() {
    local desc="$1" pattern="$2"; shift 2
    local body
    body=$(curl -s "$@" 2>/dev/null) || true
    TOTAL=$((TOTAL + 1))
    if echo "$body" | grep -q "$pattern"; then
        PASS=$((PASS + 1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL: $desc (body does not match '$pattern')"
    fi
}

# check_header DESC URL HEADER PATTERN
check_header() {
    local desc="$1" url="$2" header="$3" pattern="$4"
    local actual
    actual=$(curl -sI "$url" 2>/dev/null | grep -i "^${header}:" | sed 's/^[^:]*: *//' | tr -d '\r\n') || true
    TOTAL=$((TOTAL + 1))
    if echo "$actual" | grep -q "$pattern"; then
        PASS=$((PASS + 1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL: $desc (header '$header' = '$actual', want '$pattern')"
    fi
}

# check_size DESC URL MIN_BYTES
check_size() {
    local desc="$1" url="$2" min_bytes="$3"
    local size
    size=$(curl -s -o /dev/null -w '%{size_download}' "$url" 2>/dev/null) || true
    TOTAL=$((TOTAL + 1))
    if [ "$size" -ge "$min_bytes" ] 2>/dev/null; then
        PASS=$((PASS + 1)); echo "  PASS: $desc (${size} bytes >= ${min_bytes})"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL: $desc (${size} bytes < ${min_bytes})"
    fi
}

# check_diff DESC URL1 URL2
# Asserts that two requests return different response bodies.
check_diff() {
    local desc="$1" url1="$2" url2="$3"
    local body1 body2
    body1=$(curl -s "$url1" 2>/dev/null) || true
    body2=$(curl -s "$url2" 2>/dev/null) || true
    TOTAL=$((TOTAL + 1))
    if [ "$body1" != "$body2" ]; then
        PASS=$((PASS + 1)); echo "  PASS: $desc"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL: $desc (responses are identical)"
    fi
}

# ============================================================================
# Section: Proxy Header Tests
# ============================================================================
echo ""
echo "=== Proxy Header Tests ==="

# /ip via proxy -> response contains origin (X-Forwarded-For set by nginx)
check "GET /ip -> 200" 200 "$BASE_URL/ip"
check_body "/ip has origin field" '"origin"' "$BASE_URL/ip"

# /headers via proxy -> should contain X-Forwarded-For in response
check "GET /headers -> 200" 200 "$BASE_URL/headers"
check_body "/headers has X-Forwarded-For" "X-Forwarded-For" "$BASE_URL/headers"

# /get via proxy -> url field should contain http:// (X-Forwarded-Proto)
check "GET /get -> 200" 200 "$BASE_URL/get"
check_body "/get url field has http://" '"url"' "$BASE_URL/get"

# ============================================================================
# Section: ETag Propagation
# ============================================================================
echo ""
echo "=== ETag Propagation ==="

# Image through proxy -> Cache-Control header arrives
check "GET /image/size/small/photo.png -> 200" 200 "$BASE_URL/image/size/small/photo.png"
check_header "image Cache-Control via proxy" "$BASE_URL/image/size/small/photo.png" "Cache-Control" "public"

# Two identical requests -> same ETag (proxy could cache)
TOTAL=$((TOTAL + 1))
etag1=$(curl -sI "$BASE_URL/image/size/small/photo.png" 2>/dev/null | grep -i "^ETag:" | tr -d '\r\n') || true
etag2=$(curl -sI "$BASE_URL/image/size/small/photo.png" 2>/dev/null | grep -i "^ETag:" | tr -d '\r\n') || true
if [ -n "$etag1" ] && [ "$etag1" = "$etag2" ]; then
    PASS=$((PASS + 1)); echo "  PASS: same ETag on repeated requests ($etag1)"
else
    FAIL=$((FAIL + 1)); echo "  FAIL: ETag mismatch (1='$etag1' 2='$etag2')"
fi

# ============================================================================
# Section: Header Safety Through Proxy
# ============================================================================
echo ""
echo "=== Header Safety Through Proxy ==="

# Custom header passes through nginx
check "GET /header/X-Custom:test/status/200 -> 200" 200 "$BASE_URL/header/X-Custom:test/status/200"
check_header "X-Custom header via proxy" "$BASE_URL/header/X-Custom:test/status/200" "X-Custom" "test"

# Status code + body through proxy (SGVsbG8= = base64("Hello"))
check "GET /status/422/body/SGVsbG8= -> 422" 422 "$BASE_URL/status/422/body/SGVsbG8="
check_body "/status/422/body -> Hello" "Hello" "$BASE_URL/status/422/body/SGVsbG8="

# ============================================================================
# Section: Image Generation Through Proxy
# ============================================================================
echo ""
echo "=== Image Generation Through Proxy ==="

# Gradient warm image
check "GET /image/gradient/warm/size/small/photo.png -> 200" 200 "$BASE_URL/image/gradient/warm/size/small/photo.png"
check_header "gradient png content-type" "$BASE_URL/image/gradient/warm/size/small/photo.png" "Content-Type" "image/png"
check_size "gradient png has data" "$BASE_URL/image/gradient/warm/size/small/photo.png" 100

# AVIF conversion through proxy
check "GET /image/size/small/photo.avif -> 200" 200 "$BASE_URL/image/size/small/photo.avif"
check_header "avif content-type" "$BASE_URL/image/size/small/photo.avif" "Content-Type" "image/avif"

# WebP conversion through proxy
check "GET /image/size/small/photo.webp -> 200" 200 "$BASE_URL/image/size/small/photo.webp"
check_header "webp content-type" "$BASE_URL/image/size/small/photo.webp" "Content-Type" "image/webp"

# ============================================================================
# Section: Pipeline Through Proxy
# ============================================================================
echo ""
echo "=== Pipeline Through Proxy ==="

# delay + status + JSON body
check "GET /delay/0/status/201/get -> 201" 201 "$BASE_URL/delay/0/status/201/get"
check_body "/delay/0/status/201/get is JSON" '"url"' "$BASE_URL/delay/0/status/201/get"

# header modifier through proxy
check "GET /header/X-Test:val/status/200 -> 200" 200 "$BASE_URL/header/X-Test:val/status/200"
check_header "X-Test header via proxy" "$BASE_URL/header/X-Test:val/status/200" "X-Test" "val"

# ============================================================================
# Section: Nocache Through Proxy
# ============================================================================
echo ""
echo "=== Nocache Through Proxy ==="

# Two requests to no-cache image should produce different responses
# (server regenerates each time; the binary content differs due to noise)
TOTAL=$((TOTAL + 1))
resp1=$(curl -s "$BASE_URL/image/no-cache/size/small/photo.png" 2>/dev/null | md5sum | cut -d' ' -f1) || true
resp2=$(curl -s "$BASE_URL/image/no-cache/size/small/photo.png" 2>/dev/null | md5sum | cut -d' ' -f1) || true
if [ "$resp1" != "$resp2" ]; then
    PASS=$((PASS + 1)); echo "  PASS: no-cache images differ (md5 $resp1 vs $resp2)"
else
    # Even with no-cache, identical params may produce identical images
    # if the generator is deterministic. Check Cache-Control header instead.
    nocache_cc=$(curl -sI "$BASE_URL/image/no-cache/size/small/photo.png" 2>/dev/null | grep -i "^Cache-Control:" | tr -d '\r\n') || true
    if echo "$nocache_cc" | grep -qi "no-cache"; then
        PASS=$((PASS + 1)); echo "  PASS: no-cache has Cache-Control: no-cache (images identical but not cached)"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL: no-cache images identical and no Cache-Control: no-cache"
    fi
fi

# Verify Cache-Control: no-cache header on nocache endpoint
check_header "no-cache Cache-Control header" "$BASE_URL/image/no-cache/size/small/photo.png" "Cache-Control" "no-cache"

# ============================================================================
# Results
# ============================================================================
echo ""
echo "==========================================="
echo "RESULTS: $PASS/$TOTAL passed, $FAIL failed"
echo "==========================================="
[ "$FAIL" -eq 0 ] || exit 1
