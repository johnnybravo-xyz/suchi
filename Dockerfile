# Suchi ships two runtime targets:
#   standard: broad format support with Tesseract OCR
#   full:     the standard application plus OCRmyPDF archives
#
# Toolchain images are digest-pinned so a release can be rebuilt from its tag.

# Hold Rust until N-1 includes the 1.98.1 vtable-miscompilation fix.
ARG RUST_IMAGE=rust:1.97.1-alpine@sha256:3c38f3f82c2f3d73da3b38e18d279393a04cb43ddded0e35088a8c3324d40900
ARG ALPINE_IMAGE=alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
ARG GO_IMAGE=golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc
ARG DEBIAN_IMAGE=debian:bookworm-20260803-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

# Build the pinned anydoc converter once for both runtime images. The readable
# tag is audited by hack/pin-bumper.sh; the resolved commit controls checkout.
FROM ${RUST_IMAGE} AS anydoc-build
ARG ANYDOC_TAG=v0.2.3
ARG ANYDOC_COMMIT=bf3d33e61731580d1ee1c6a85e56093d715a21a6
ARG TARGETARCH

RUN apk add --no-cache git musl-dev pkgconfig
WORKDIR /src

RUN git init . && \
    git remote add origin https://github.com/firecrawl/anydoc.git && \
    git fetch --depth 1 origin "${ANYDOC_COMMIT}" && \
    git checkout --detach FETCH_HEAD && \
    test "$(git rev-parse HEAD)" = "${ANYDOC_COMMIT}"

RUN --mount=type=cache,id=anydoc-cargo-${TARGETARCH},target=/var/cache/cargo,sharing=locked \
    --mount=type=cache,id=anydoc-target-${TARGETARCH},target=/src/target,sharing=locked \
    CARGO_HOME=/var/cache/cargo cargo build --locked --release --example convert && \
    cp target/release/examples/convert /out-anydoc

RUN strip /out-anydoc && \
    if [ -s LICENSE ]; then cp LICENSE /out-anydoc-license; \
    elif [ -s LICENSE-MIT ]; then cp LICENSE-MIT /out-anydoc-license; \
    else echo "anydoc license file not found" >&2; exit 1; fi

# Extract the Perl library used by the standard image to read Outlook .msg
# files. Keeping this in a build stage avoids shipping wget and the tarball.
FROM ${ALPINE_IMAGE} AS msgconvert-build
ARG MSGCONVERT_VERSION=0.921
ARG MSGCONVERT_SHA256=fb4abeea14cda51c1e60bc211cf9521bbbaae74b84118ca6d58f0a7223680388

RUN apk add --no-cache ca-certificates
WORKDIR /src

RUN wget -q -O source.tar.gz \
      "https://cpan.metacpan.org/authors/id/M/MV/MVZ/Email-Outlook-Message-${MSGCONVERT_VERSION}.tar.gz" && \
    echo "${MSGCONVERT_SHA256}  source.tar.gz" | sha256sum -c - && \
    tar -xzf source.tar.gz && \
    mkdir -p /out && \
    cp -R "Email-Outlook-Message-${MSGCONVERT_VERSION}/lib/Email/Outlook" /out/Outlook

# Compile the static Suchi binary. The web app is already built and committed
# under core/ui/spa/dist; make ui-check and release preflight verify that copy.
FROM ${GO_IMAGE} AS build
ARG VERSION=dev
ARG REVISION

WORKDIR /src

COPY go.mod go.sum go.work go.work.sum* ./
COPY plugin-api plugin-api
COPY core core
COPY plugins plugins
COPY distro distro
COPY hack hack

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" -o /out/suchi ./distro/cmd/suchi

# Full runtime: Debian packages OCRmyPDF and its archive-processing stack.
FROM ${DEBIAN_IMAGE} AS full

# Bootstrap TLS before downloading the runtime toolchain. Debian's signed
# indexes still verify package hashes, while HTTPS avoids stale proxy payloads.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
      djvulibre-bin \
      imagemagick \
      libemail-address-perl \
      libemail-outlook-message-perl \
      libheif1 \
      ocrmypdf \
      poppler-utils \
      qpdf \
      tesseract-ocr \
    && rm -rf /var/lib/apt/lists/*

# Suchi encodes raster images as PDFs; PDF decoding (including coder aliases)
# stays with Poppler.
RUN sed -i 's|<policy domain="coder" rights="none" pattern="PDF" />|<policy domain="coder" rights="write" pattern="{PDF,PDFA,AI,EPDF,POCKETMOD}" />|; /<\/policymap>/i\  <policy domain="coder" rights="none" pattern="{PS,PS2,PS3,EPS,EPS2,EPS3,EPSF,EPSI,EPI,XPS}" />' /etc/ImageMagick-6/policy.xml
RUN useradd -u 65532 -m -s /usr/sbin/nologin suchi && \
    mkdir -p /data && chown 65532:65532 /data

COPY --from=build /out/suchi /usr/local/bin/suchi
COPY --from=anydoc-build /out-anydoc /usr/local/bin/anydoc
COPY --from=anydoc-build /out-anydoc-license /usr/local/share/licenses/anydoc/LICENSE
COPY LICENSE /usr/local/share/licenses/suchi/LICENSE

RUN ln -s suchi /usr/local/bin/suchi-mcp

USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000 OCR_ENGINE=ocrmypdf

ENTRYPOINT ["/usr/local/bin/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/usr/local/bin/suchi", "healthcheck"]

# Standard runtime: keep this last so `docker build .` produces the default
# image without requiring an explicit target.
FROM ${ALPINE_IMAGE} AS standard
RUN apk add --no-cache \
      ca-certificates \
      djvulibre \
      imagemagick \
      imagemagick-heic \
      imagemagick-pdf \
      perl \
      perl-email-mime \
      perl-io-string \
      perl-ole-storage_lite \
      poppler-utils \
      qpdf \
      tesseract-ocr \
      tesseract-ocr-data-eng

RUN sed -i '/<\/policymap>/i\  <policy domain="coder" rights="write" pattern="{PDF,PDFA,AI,EPDF,POCKETMOD}" />\n  <policy domain="coder" rights="none" pattern="{PS,PS2,PS3,EPS,EPS2,EPS3,EPSF,EPSI,EPI,XPS}" />' /etc/ImageMagick-7/policy.xml

RUN adduser -D -u 65532 -s /sbin/nologin suchi && \
    mkdir -p /data && chown 65532:65532 /data

COPY --from=build /out/suchi /usr/local/bin/suchi
COPY --from=anydoc-build /out-anydoc /usr/local/bin/anydoc
COPY --from=anydoc-build /out-anydoc-license /usr/local/share/licenses/anydoc/LICENSE
COPY LICENSE /usr/local/share/licenses/suchi/LICENSE
COPY --from=msgconvert-build /out/Outlook /usr/local/share/perl5/site_perl/Email/Outlook
COPY packaging/msgconvert/msgconvert /usr/local/bin/msgconvert
COPY packaging/msgconvert/NOTICE /usr/local/share/doc/suchi-msgconvert/NOTICE

RUN ln -s suchi /usr/local/bin/suchi-mcp

USER 65532:65532
EXPOSE 8000
VOLUME ["/data"]
ENV DATA_DIR=/data LISTEN_ADDR=:8000 OCR_ENGINE=tesseract

ENTRYPOINT ["/usr/local/bin/suchi"]
CMD ["serve"]
HEALTHCHECK --interval=30s --retries=3 CMD ["/usr/local/bin/suchi", "healthcheck"]
