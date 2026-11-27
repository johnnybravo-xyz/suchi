#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Reject stale release versions and prerelease image channels in active docs."""

from __future__ import annotations

import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def read(relative: str) -> str:
    return (ROOT / relative).read_text(encoding="utf-8")


def main() -> int:
    source = read("distro/cmd/suchi/main.go")
    match = re.search(r'const developmentVersion = "(v[^"]+)-dev"', source)
    if match is None:
        print("cannot derive the current release from developmentVersion", file=sys.stderr)
        return 1
    version = match.group(1)

    schema_source = read("core/db/adopt.go")
    schema_match = re.search(r"StableSchemaVersion\s*=\s*(\d+)", schema_source)
    fingerprint_match = re.search(
        r'stableFingerprint\s*=\s*"([0-9a-f]{64})"', schema_source
    )
    if schema_match is None or fingerprint_match is None:
        print("cannot derive the current schema identity", file=sys.stderr)
        return 1
    schema_version = schema_match.group(1)
    schema_fingerprint = fingerprint_match.group(1)

    required = {
        "README.md": (
            f"ghcr.io/johnnybravo-xyz/suchi:{version}",
            f"{version}-full",
        ),
        "docs/getting-started.mdx": (
            f"ghcr.io/johnnybravo-xyz/suchi:{version}",
            f"{version}-full",
        ),
        "docs/release-process.mdx": (
            f"export VERSION={version}",
            f"(current schema) | `{schema_fingerprint}` |",
            f"| Stable schema {schema_version} | Exact current fingerprint above with `stable-v1` | No-op |",
        ),
        "compose.yaml": ("ghcr.io/johnnybravo-xyz/suchi:latest",),
        "deploy/k8s/suchi.yaml": ("ghcr.io/johnnybravo-xyz/suchi:latest",),
        "docs/architecture-storage.mdx": (
            f"reaches schema {schema_version}",
            f"current schema-{schema_version} database",
            f"canonical schema-{schema_version}",
        ),
    }

    errors: list[str] = []
    for relative, values in required.items():
        text = read(relative)
        for value in values:
            if value not in text:
                errors.append(f"{relative}: missing current release reference {value!r}")


    active_docs = [ROOT / "README.md", *sorted((ROOT / "docs").rglob("*.mdx"))]
    stale_image = re.compile(
        r"ghcr\.io/johnnybravo-xyz/suchi:(?:v\d+\.\d+\.\d+-(?:beta|rc)[^\s`]*|beta(?:-standard|-full)?|rc(?:-full)?)"
    )
    release_image = re.compile(r"ghcr\.io/johnnybravo-xyz/suchi:(v\d+\.\d+\.\d+)(?:-full)?\b")
    for path in active_docs:
        if path.name == "release-process.mdx":
            continue
        for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            match = stale_image.search(line)
            if match is not None:
                errors.append(
                    f"{path.relative_to(ROOT)}:{line_number}: stale release channel {match.group(0)!r}"
                )
            for tag in release_image.findall(line):
                if tag != version:
                    errors.append(
                        f"{path.relative_to(ROOT)}:{line_number}: stale release image {tag!r}; use {version}"
                    )

    if errors:
        print("release documentation check failed:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1
    print(f"release documentation references {version} and current image channels")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
