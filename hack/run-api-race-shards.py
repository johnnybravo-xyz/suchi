#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Run core/api race tests in two equal-count package processes."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path


API_PACKAGE = "./core/api"
SHARD_COUNT = 2
ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    listed = subprocess.run(
        ["go", "test", "-race", "-list", "^Test", API_PACKAGE],
        check=True,
        capture_output=True,
        cwd=ROOT,
        text=True,
    )
    names = [line for line in listed.stdout.splitlines() if line.startswith("Test")]
    if not names:
        print(f"no tests found in {API_PACKAGE}", file=sys.stderr)
        return 1

    processes: list[subprocess.Popen[bytes]] = []
    for index in range(SHARD_COUNT):
        shard = names[index::SHARD_COUNT]
        pattern = "^(" + "|".join(shard) + ")$"
        print(f"=== release test core/api shard {index + 1}/{SHARD_COUNT} ({len(shard)} tests) ===", flush=True)
        processes.append(
            subprocess.Popen(
                ["go", "test", *sys.argv[1:], "-run", pattern, API_PACKAGE],
                cwd=ROOT,
            )
        )

    failed = False
    try:
        for process in processes:
            failed = process.wait() != 0 or failed
    except KeyboardInterrupt:
        for process in processes:
            process.terminate()
        raise
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
