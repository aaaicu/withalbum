# WithAlbum Server container image (docs/DOCKER.md).
#
#   docker compose build          # or: docker build -t withalbum-server:local .
#
# The image runs as an unprivileged user, holds no secrets and no data: the
# storage is a bind mount at /data. Preview tools: ffmpeg (videos, playback
# copies) and libheif's heif-dec (HEIC/HEIF/AVIF stills; there is no sips
# on Linux).

ARG GO_VERSION=1.27
ARG DEBIAN_RELEASE=trixie

FROM golang:${GO_VERSION}-${DEBIAN_RELEASE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/withalbum-server ./server

FROM debian:${DEBIAN_RELEASE}-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg libheif-examples ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /nonexistent --shell /usr/sbin/nologin withalbum \
 && mkdir /data && chown 10001:10001 /data
COPY --from=build /out/withalbum-server /usr/local/bin/withalbum-server
COPY LICENSE NOTICE /usr/share/doc/withalbum-server/
ENV WITHALBUM_CONTAINER=1 \
    WITHALBUM_ROOT=/data \
    WITHALBUM_LISTEN=0.0.0.0:8787 \
    WITHALBUM_FUNNEL_LISTEN=0.0.0.0:8788 \
    HOME=/tmp
USER 10001:10001
WORKDIR /data
EXPOSE 8787 8788
ENTRYPOINT ["/usr/local/bin/withalbum-server"]
CMD ["-tls", "-allow-lan"]
