# The build stage runs on the build machine's own CPU and cross-compiles for
# the target platform, so arm64 and amd64 images build on any machine without
# emulation. The final stage has no RUN steps, so nothing ever executes on
# the target platform during the build either.
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
ARG TARGETOS
ARG TARGETARCH
# The commit being built, reported by /api/status. CI passes it; .git is not
# in the build context, so Go cannot stamp it itself.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
# CGO_ENABLED=0 builds a static binary, possible because the SQLite driver is
# pure Go. -s -w drops the symbol table and debug info; stack traces and
# profiles still name functions.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/synthetic-monitor ./cmd/synthetic-monitor \
    && mkdir /out/data

# distroless/static has no shell or package manager, only CA certificates
# (which HTTPS and TLS probes need), timezone data and a non-root user.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
LABEL org.opencontainers.image.source=https://github.com/MSJawad/synthetic-monitor \
      org.opencontainers.image.description="Synthetic monitoring: probes, SLOs and alerts"
COPY --from=build /out/synthetic-monitor /usr/local/bin/
# The history database's directory, owned by the runtime user so a volume
# mounted here starts out writable.
COPY --from=build --chown=65532:65532 /out/data /data
WORKDIR /data
# Numeric, so Kubernetes can verify runAsNonRoot without reading /etc/passwd.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/synthetic-monitor"]
