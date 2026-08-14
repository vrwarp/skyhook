# Landside server image: Go binary plus a real Chromium and the image encoders.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/skyhookd ./cmd/skyhookd && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w" \
      -o /out/skyhookctl ./cmd/skyhookctl

FROM debian:bookworm-slim
# chromium is the whole point of the landside half; libavif/webp give the
# transcoder its good codecs, and xvfb covers sites that fight headless.
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium \
      ca-certificates \
      fonts-liberation fonts-noto-core fonts-noto-color-emoji \
      libavif-bin webp \
      xvfb \
      tini \
      dumb-init \
    && rm -rf /var/lib/apt/lists/*

# Chromium's own sandbox needs a non-root user; the profile holds real cookies,
# so it gets its own uid and nothing else shares it.
RUN useradd --create-home --shell /usr/sbin/nologin skyhook
COPY --from=build /out/skyhookd /usr/local/bin/skyhookd
COPY --from=build /out/skyhookctl /usr/local/bin/skyhookctl
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER skyhook
ENV SKYHOOK_DATA_DIR=/data \
    SKYHOOK_CHROME=/usr/bin/chromium \
    SKYHOOK_LISTEN=:4433 \
    SKYHOOK_FALLBACK_LISTEN=:4434
VOLUME ["/data"]
EXPOSE 4433/udp 4434/tcp
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
