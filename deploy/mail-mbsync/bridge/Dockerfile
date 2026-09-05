# Proton Mail Bridge image. shenxn base + libfido2 for Bridge 3.25+
# (base image ships an older Bridge that doesn't need it; Bridge's
# auto-updater fetches 3.25+ on first boot and then crashes without
# libfido2.so.1).
FROM shenxn/protonmail-bridge:3.19.0-1@sha256:3717b4441130675dc9131196de9f9c5287d2ea21b138d83b0486429e1737638a

RUN apt-get update \
    && apt-get install -y --no-install-recommends libfido2-1 \
    && rm -rf /var/lib/apt/lists/*
