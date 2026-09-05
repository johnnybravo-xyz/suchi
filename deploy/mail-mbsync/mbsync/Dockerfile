# mbsync + fan-to-ingest image. Debian slim + isync + gettext (for
# envsubst on the mbsyncrc template). Small (~50 MB).
FROM debian:bookworm-20260803-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       isync \
       ca-certificates \
       gettext-base \
       findutils \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
