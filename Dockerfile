# suchi — two-stage build with -slim and full targets.
#
# slim: alpine + qpdf + poppler-utils + tesseract + anydoc. Covers the
#   entire PDF ingest path (text-native shortcut, tessocr scanned path,
#   qpdf normalization, ZUGFeRD invoice extraction) plus office document
#   text extraction (docx, xlsx, pptx, odt, rtf, csv). No searchable-PDF
#   archive on scanned PDFs — that comes with ocrmypdf, which lives in
#   the full image. Approx ~80 MB.
#
# full: adds ocrmypdf (searchable-PDF archives = text-selectable scanned
#   PDFs) + djvulibre-bin (DjVu text extraction) + msgconvert (Outlook
#   .msg → .eml). Approx ~400 MB.

# ---------- anydoc build stage (Phase 3.5) ----------
# Firecrawl publishes anydoc as a Rust library on crates.io + Node/Python
# bindings, but not as a standalone CLI binary. The CLI lives in the
# repo as `examples/convert.rs`. We compile the example as a static musl
# binary and rename it "anydoc" — same pattern the pin-bumper script
# understands. Approx ~10 MB output; both slim and full copy it in.
#
# Interface stability CAVEAT: examples/ is not upstream-guaranteed as a
# stable CLI. Argv shape is `<file> [-f <fmt>] [-o <out>] [--assets dir]`
# as of v0.1.3; if a future bump changes this shape, the extractor in
# core/pipeline/anydoc/ needs a matching update. `hack/pin-bumper.sh`
# calls this out at every bump.
#
# Bump procedure:
#   1. hack/pin-bumper.sh reports "BUMP suggested: vX → vY"
#   2. Read the upstream compare link it prints; scan for argv changes
#      in examples/convert.rs
#   3. Update ANYDOC_TAG below
#   4. Rebuild slim; sanity-check `docker run --rm suchi:slim doctor`
#      lists anydoc, and a smoke docx ingests to non-empty content
FROM rust:1-alpine AS anydoc-build
ARG ANYDOC_TAG=v0.1.3
RUN apk add --no-cache git musl-dev pkgconfig
WORKDIR /src
RUN git clone --depth 1 --branch "${ANYDOC_TAG}" \
      https://github.com/firecrawl/anydoc.git .
# rust:1-alpine's toolchain is musl-native, so the default target is
# already static musl — no --target flag needed. LTO + strip via the
# release profile in anydoc's own Cargo.toml.
RUN cargo build --release --example convert
RUN cp target/release/examples/convert /out-anydoc && strip /out-anydoc

# ---------- build stage ----------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum* ./
COPY plugin-api plugin-api
COPY core core
COPY plugins plugins
COPY distro distro
# hack/ holds local dev tools referenced from go.work (fixture generator,
# ingest driver). Not built into the binary — just needs to be present so
# `go build` can load the workspace without complaining about missing modules.
COPY hack hack
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
      tesseract-ocr-data-eng \
      imagemagick \
      imagemagick-heic
RUN adduser -D -u 65532 -s /sbin/nologin suchi && \
    mkdir -p /data && chown 65532:65532 /data
COPY --from=build /out/suchi /usr/local/bin/suchi
COPY --from=anydoc-build /out-anydoc /usr/local/bin/anydoc
# Argv[0] dispatch: `suchi-mcp` invokes the MCP subcommand. Ships the
# ergonomic name for local agent configs (`command: "suchi-mcp"`).
RUN ln -s suchi /usr/local/bin/suchi-mcp
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
      imagemagick \
      libheif1 \
      libemail-outlook-message-perl \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Debian's ImageMagick policy.xml blocks HEIC by default. Enable it —
# we only need HEIC decode for photograph-of-document ingestion.
RUN sed -i 's|<policy domain="coder" rights="none" pattern="HEIC" />||g; s|<policy domain="coder" rights="none" pattern="HEIF" />||g' /etc/ImageMagick-6/policy.xml || true
RUN useradd -u 65532 -m -s /usr/sbin/nologin suchi
COPY --from=build /out/suchi /usr/local/bin/suchi
COPY --from=anydoc-build /out-anydoc /usr/local/bin/anydoc
# Argv[0] dispatch: `suchi-mcp` invokes the MCP subcommand. Ships the
# ergonomic name for local agent configs (`command: "suchi-mcp"`).
RUN ln -s suchi /usr/local/bin/suchi-mcp
RUN mkdir -p /data && chown 65532:65532 /data
USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000 OCR_ENGINE=ocrmypdf
ENTRYPOINT ["/usr/local/bin/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/usr/local/bin/suchi", "healthcheck"]
