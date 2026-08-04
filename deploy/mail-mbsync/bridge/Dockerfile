# Proton Mail Bridge image. shenxn base + libfido2 for Bridge 3.25+
# (base image ships an older Bridge that doesn't need it; Bridge's
# auto-updater fetches 3.25+ on first boot and then crashes without
# libfido2.so.1).
FROM shenxn/protonmail-bridge:latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends libfido2-1 \
    && rm -rf /var/lib/apt/lists/*
