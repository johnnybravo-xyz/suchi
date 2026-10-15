<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Suchi Desktop

This directory is the native launcher for the canonical Suchi web application. It does not contain a second frontend: the Go sidecar embeds `ui/`, while `launcher/` only renders lifecycle, setup, and recovery states before the sidecar is ready.

Use the root Make targets to build and check the launcher. The build creates a target-suffixed sidecar and matching identity manifest under ignored `src-tauri/binaries/` and `src-tauri/generated/` directories before invoking Cargo or Tauri.
