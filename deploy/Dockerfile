# Landside server image: the Go binary, a real Chromium, the image encoders, and
# the plane-side PWA the server hands to the browser.
FROM node:26-bookworm-slim AS client
WORKDIR /client
COPY client/package.json client/package-lock.json ./
RUN npm ci
COPY client/ ./
RUN npm run build

FROM golang:1.27-bookworm AS build
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
# transcoder its good codecs, and xvfb is how it runs headful — which is the
# default, because headless Chromium tells every site that it is one.
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
# so it gets its own uid and nothing else shares it. The uid is pinned because a
# bind-mounted data directory has to be chown'd to something, and "10001" is a
# better answer than "whatever useradd picked this build".
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin skyhook
# The data directory must exist in the image and be owned by that uid: Docker
# creates a missing volume mountpoint as root, and the server then cannot write
# its own profile. A fresh volume inherits this ownership.
RUN install -d -o skyhook -g skyhook -m 0700 /data
COPY --from=build /out/skyhookd /usr/local/bin/skyhookd
COPY --from=build /out/skyhookctl /usr/local/bin/skyhookctl
COPY --from=client /client/dist /usr/share/skyhook/webapp
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER skyhook
ENV SKYHOOK_DATA_DIR=/data \
    SKYHOOK_CHROME=/usr/bin/chromium \
    SKYHOOK_LISTEN=:4433 \
    SKYHOOK_FALLBACK_LISTEN=:4434 \
    SKYHOOK_WEB_ROOT=/usr/share/skyhook/webapp
VOLUME ["/data"]
EXPOSE 4433/udp 4434/tcp
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
