# syntax = docker/dockerfile:1.3
FROM golang:1.26 AS build

WORKDIR /go/src/github.com/mccutchen/go-httpbin

COPY . .

RUN --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    make build buildtests

FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
    libavif-bin webp imagemagick ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN useradd -r -s /usr/sbin/nologin appuser
USER appuser

COPY --from=build /go/src/github.com/mccutchen/go-httpbin/dist/go-httpbin* /bin/

EXPOSE 8080
CMD ["/bin/go-httpbin"]
