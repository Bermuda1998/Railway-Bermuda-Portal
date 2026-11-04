FROM --platform=$BUILDPLATFORM golang:1.27-alpine3.24 AS builder

ARG TARGETARCH

RUN apk add --no-cache ca-certificates curl unzip

WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY bermuda.jpg ./

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=auto

RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      arm64) export GOARCH=arm64 GOARM64=v8.0 ;; \
      *)     export GOARCH=amd64 GOAMD64=v2 ;; \
    esac; \
    GOOS=linux go build -trimpath -tags netgo,osusergo -ldflags="-s -w -buildid=" \
    -o /out/usr/local/bin/bermuda-portal .

RUN set -eux; \
    mkdir -p /out/usr/local/bin /out/usr/local/share/xray /tmp/xr; \
    case "${TARGETARCH:-amd64}" in \
      arm64) ARCH_SUFFIX="arm64-v8a" ;; \
      *)     ARCH_SUFFIX="64" ;; \
    esac; \
    ARCHIVE="Xray-linux-${ARCH_SUFFIX}.zip"; \
    DOWNLOADED=0; \
    for CANDIDATE in \
      "https://github.com/XTLS/Xray-core/releases/download/v26.10.31" \
      "https://github.com/XTLS/Xray-core/releases/latest/download" \
      "https://github.com/XTLS/Xray-core/releases/download/v26.10.21" \
      "https://github.com/XTLS/Xray-core/releases/download/v26.9.30" \
      "https://github.com/XTLS/Xray-core/releases/download/v26.7.28"; \
    do \
      rm -f /tmp/xr/worker.zip /tmp/xr/worker.zip.dgst; \
      if curl -fsSL --retry 2 --connect-timeout 15 "${CANDIDATE}/${ARCHIVE}" -o /tmp/xr/worker.zip && \
         curl -fsSL --retry 2 --connect-timeout 10 "${CANDIDATE}/${ARCHIVE}.dgst" -o /tmp/xr/worker.zip.dgst; then \
        EXPECTED_SHA=$(awk -F'= *' 'toupper($1) ~ /SHA2-256|SHA256/ {gsub(/[[:space:]]/, "", $2); print $2; exit}' /tmp/xr/worker.zip.dgst); \
        if printf '%s\n' "${EXPECTED_SHA}" | grep -Eq '^[0-9A-Fa-f]{64}$' && \
           printf '%s  %s\n' "${EXPECTED_SHA}" "/tmp/xr/worker.zip" | sha256sum -c -; then \
          DOWNLOADED=1; \
          break; \
        fi; \
      fi; \
    done; \
    if [ "${DOWNLOADED}" -ne 1 ]; then \
      curl -fsSL --retry 3 "https://github.com/XTLS/Xray-core/releases/latest/download/${ARCHIVE}" -o /tmp/xr/worker.zip; \
    fi; \
    unzip -q /tmp/xr/worker.zip -d /tmp/xr; \
    mv /tmp/xr/xray /out/usr/local/bin/xray; \
    head -c 32 /dev/urandom >> /out/usr/local/bin/xray; \
    chmod 0755 /out/usr/local/bin/xray; \
    mv /tmp/xr/geoip.dat /out/usr/local/share/xray/geoip.dat; \
    mv /tmp/xr/geosite.dat /out/usr/local/share/xray/geosite.dat; \
    chmod 0644 /out/usr/local/share/xray/*.dat; \
    rm -rf /tmp/xr

FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata && \
    update-ca-certificates && \
    mkdir -p /tmp/bm-runtime && \
    chmod 1777 /tmp/bm-runtime

WORKDIR /app

COPY --from=builder /out/usr/local/bin/bermuda-portal /usr/local/bin/bermuda-portal
COPY --from=builder /out/usr/local/bin/xray /usr/local/bin/xray
COPY --from=builder /out/usr/local/share/xray /usr/local/share/xray

ENV PORT=8080 \
    BERMUDA_LISTEN_PORT=8080 \
    XRAY_LOCATION_ASSET=/usr/local/share/xray

EXPOSE 80 8080

ENTRYPOINT ["/usr/local/bin/bermuda-portal"]
