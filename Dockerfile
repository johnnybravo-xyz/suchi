# suchi — two-stage build with -slim and full targets.
#
# slim: distroless-static, binary only. Text-native archives (post-Phase-2
#   pdf-inspector routing) work fully; scanned-PDF ingest needs a
#   sidecar OCR plugin bound over UDS.
#
# full: adds tesseract + ocrmypdf + qpdf + libreoffice-core so a single
#   image covers 100% of ingest cases. Bigger, still one process, no
#   external services.

# ---------- build stage ----------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work ./
COPY plugin-api plugin-api
COPY core core
COPY plugins plugins
COPY distro distro
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    cd distro && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/suchi ./cmd/suchi

# ---------- data-dir prep ----------
# Bootstrapper stage: create /data owned by UID 65532 so the distroless
# slim image can inherit it. Named-volume mounts (and empty bind mounts)
# preserve directory ownership from the image, so the non-root process
# can create dms.db on first boot without an entrypoint chown dance.
FROM debian:bookworm-slim AS data-prep
RUN mkdir -p /data && chown 65532:65532 /data

# ---------- slim stage ----------
FROM gcr.io/distroless/static-debian12:nonroot AS slim
COPY --from=build /out/suchi /suchi
COPY --from=data-prep --chown=65532:65532 /data /data
USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000
ENTRYPOINT ["/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/suchi", "healthcheck"]

# ---------- full stage (adds OCR + office deps) ----------
# Debian slim so we can apt-install tesseract/qpdf/etc. Still one process.
FROM debian:bookworm-slim AS full
RUN apt-get update && apt-get install -y --no-install-recommends \
      tesseract-ocr \
      ocrmypdf \
      qpdf \
      poppler-utils \
      libreoffice-core \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -u 65532 -m -s /usr/sbin/nologin suchi
COPY --from=build /out/suchi /usr/local/bin/suchi
RUN mkdir -p /data && chown 65532:65532 /data
USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000
ENTRYPOINT ["/usr/local/bin/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/usr/local/bin/suchi", "healthcheck"]
