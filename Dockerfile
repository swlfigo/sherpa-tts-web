# syntax=docker/dockerfile:1

# The speech engine, sherpa-onnx, is a C library linked with cgo; its
# prebuilt shared libraries come with the Go module and ship next to the
# program, found through the rpath $ORIGIN/lib.
#
# It builds on AlmaLinux 8 so that the program needs only glibc 2.28: the
# binary release then runs on RHEL/AlmaLinux/Rocky 8+, Alibaba Cloud Linux 3,
# Ubuntu 20.04+ and Debian 10+, like the sherpa-onnx libraries themselves.
FROM almalinux:8 AS build
ARG TARGETARCH
ARG VERSION=dev
ARG GO_VERSION=1.26.3
RUN dnf -y install gcc tar gzip && dnf clean all \
 && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) triple=x86_64-unknown-linux-gnu ;; \
      arm64) triple=aarch64-unknown-linux-gnu ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    lib="$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)/lib/$triple"; \
    mkdir -p /out/lib; \
    cp "$lib/libsherpa-onnx-c-api.so" "$lib/libonnxruntime.so" /out/lib/; \
    CGO_ENABLED=1 CGO_LDFLAGS='-Wl,-rpath,$ORIGIN/lib' \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/sherpa-tts-web .; \
    cp LICENSE THIRD_PARTY_NOTICES.md /out/

# The program and its libraries alone:
#   docker buildx build --target dist --output type=local,dest=dist .
FROM scratch AS dist
COPY --from=build /out/ /

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data --no-create-home tts \
 && mkdir /data && chown tts /data
COPY --from=build /out/ /app/
USER tts
ENV TTS_ADDR=:8080 TTS_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/app/sherpa-tts-web"]
