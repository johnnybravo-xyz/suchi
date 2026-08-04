# suchi — two-stage build with -slim and full targets.
#
# slim: alpine + qpdf + poppler-utils + tesseract. Covers the entire
#   PDF ingest path (text-native shortcut, tessocr scanned path, qpdf
#   normalization, ZUGFeRD invoice extraction). No searchable-PDF
#   archive on scanned PDFs — that comes with ocrmypdf, which lives in
#   the full image. Approx ~70 MB.
#
# full: adds ocrmypdf (searchable-PDF archives) + djvulibre-bin (DjVu)
#   + libreoffice-core (planned office-doc converter). Approx ~1 GB.

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

# ---------- slim stage (PDF pipeline: tessocr, not ocrmypdf) ----------
# Alpine so we can apt-install the four binaries the PDF chain needs
# (qpdf, pdftotext, pdftoppm, tesseract) at ~70 MB total. musl-safe:
# the Go binary is built CGO_ENABLED=0 so it runs identically under
# musl and glibc.
FROM alpine:3 AS slim
RUN apk add --no-cache \
      ca-certificates \
      qpdf \
      poppler-utils \
      tesseract-ocr \
      tesseract-ocr-data-eng
RUN adduser -D -u 65532 -s /sbin/nologin suchi && \
    mkdir -p /data && chown 65532:65532 /data
COPY --from=build /out/suchi /usr/local/bin/suchi
USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000 OCR_ENGINE=tesseract
ENTRYPOINT ["/usr/local/bin/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/usr/local/bin/suchi", "healthcheck"]

# ---------- full stage (adds OCR + office deps) ----------
# Debian slim so we can apt-install tesseract/qpdf/etc. Still one process.
FROM debian:bookworm-slim AS full
RUN apt-get update && apt-get install -y --no-install-recommends \
      tesseract-ocr \
      ocrmypdf \
      qpdf \
      poppler-utils \
      djvulibre-bin \
      libreoffice-core \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -u 65532 -m -s /usr/sbin/nologin suchi
COPY --from=build /out/suchi /usr/local/bin/suchi
RUN mkdir -p /data && chown 65532:65532 /data
USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000 OCR_ENGINE=ocrmypdf
ENTRYPOINT ["/usr/local/bin/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/usr/local/bin/suchi", "healthcheck"]
