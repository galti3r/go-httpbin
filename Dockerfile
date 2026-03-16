# syntax = docker/dockerfile:1.3
FROM golang:1.26 AS build

WORKDIR /go/src/github.com/mccutchen/go-httpbin

COPY . .

RUN --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    make build buildtests

# Stage to extract image conversion tools and their shared libraries
FROM debian:bookworm-slim AS tools

RUN apt-get update && apt-get install -y --no-install-recommends \
    libavif-bin webp && \
    rm -rf /var/lib/apt/lists/*

# Collect binaries and all their shared library dependencies
RUN mkdir -p /export/bin /export/lib && \
    cp /usr/bin/avifenc /usr/bin/cwebp /export/bin/ && \
    for bin in /export/bin/*; do \
        ldd "$bin" 2>/dev/null | awk '/=>/{print $3}' | while read lib; do \
            [ -f "$lib" ] && cp "$lib" /export/lib/ 2>/dev/null || true; \
        done; \
    done

# distroless/cc includes glibc, needed for dynamically-linked tools
FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=tools /export/bin/ /usr/local/bin/
COPY --from=tools /export/lib/ /usr/local/lib/imgtools/
COPY --from=build /go/src/github.com/mccutchen/go-httpbin/dist/go-httpbin* /bin/

ENV LD_LIBRARY_PATH="/usr/local/lib/imgtools"

EXPOSE 8080
CMD ["/bin/go-httpbin"]
