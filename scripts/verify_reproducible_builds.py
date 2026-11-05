#!/usr/bin/env python3
"""Build one pinned revision twice and compare release artifacts byte for byte."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile
import zlib


TARGETS = (
    ("linux", "amd64", "tar.gz"),
    ("linux", "arm64", "tar.gz"),
    ("darwin", "arm64", "tar.gz"),
    ("windows", "amd64", "zip"),
)


def run(arguments: list[str], cwd: Path, env: dict[str, str] | None = None) -> str:
    result = subprocess.run(
        arguments,
        cwd=cwd,
        env=env,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    return result.stdout


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def expected_artifacts(version: str) -> list[str]:
    names = [
        f"restic-defensive-mcp_{version}_{target_os}_{target_arch}.{archive_type}"
        for target_os, target_arch, archive_type in TARGETS
    ]
    return sorted(names + ["SHA256SUMS"])


def build(
    repository: Path,
    revision: str,
    version: str,
    epoch: int,
    checkout: Path,
    output: Path,
    cache: Path,
    temporary: Path,
) -> dict[str, object]:
    run(["git", "worktree", "add", "--detach", "--force", str(checkout), revision], repository)
    env = os.environ.copy()
    for key in ("GOFLAGS", "GOOS", "GOARCH"):
        env.pop(key, None)
    env.update(
        {
            "CGO_ENABLED": "0",
            "GOCACHE": str(cache),
            "GOTMPDIR": str(temporary),
            "GOTOOLCHAIN": "local",
            "LANG": "C",
            "LC_ALL": "C",
            "PYTHONHASHSEED": "0",
            "SOURCE_DATE_EPOCH": str(epoch),
            "TZ": "UTC",
        }
    )
    run(
        [
            sys.executable,
            str(checkout / "scripts" / "build_release.py"),
            "--version",
            version,
            "--output-dir",
            str(output),
            "--source-date-epoch",
            str(epoch),
        ],
        checkout,
        env,
    )
    return env


def compare(version: str, first: Path, second: Path) -> list[str]:
    mismatches: list[str] = []
    for name in expected_artifacts(version):
        left = first / name
        right = second / name
        if not left.is_file() or not right.is_file():
            missing = "first" if not left.is_file() else "second"
            mismatches.append(f"{name}: missing from {missing} build")
            continue
        if left.read_bytes() != right.read_bytes():
            mismatches.append(
                f"{name}: size {left.stat().st_size} != {right.stat().st_size}; "
                f"sha256 {sha256(left)} != {sha256(right)}"
            )
    unexpected = sorted(
        path.name
        for path in first.iterdir()
        if path.is_file() and path.name not in expected_artifacts(version)
    )
    unexpected += sorted(
        path.name
        for path in second.iterdir()
        if path.is_file() and path.name not in expected_artifacts(version)
    )
    for name in sorted(set(unexpected)):
        mismatches.append(f"{name}: unexpected artifact")
    return mismatches


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--revision", default="HEAD")
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    parser.add_argument("--repository", default=".")
    arguments = parser.parse_args()

    repository = Path(arguments.repository).resolve()
    revision = run(["git", "rev-parse", arguments.revision], repository).strip()
    epoch = arguments.source_date_epoch
    if epoch < 0:
        raise ValueError("source date epoch must not be negative")

    root = Path(tempfile.mkdtemp(prefix="restic-defensive-repro-"))
    worktrees: list[Path] = []
    try:
        checkouts = [root / "checkout-a", root / "checkout-b"]
        outputs = [root / "output-a", root / "output-b"]
        caches = [root / "cache-a", root / "cache-b"]
        temporary = [root / "tmp-a", root / "tmp-b"]
        for path in outputs + temporary:
            path.mkdir(parents=True, exist_ok=True)

        envelope = {
            "source_revision": revision,
            "version": arguments.version,
            "source_date_epoch": epoch,
            "go_version": run(["go", "version"], repository).strip(),
            "python_version": platform.python_version(),
            "zlib_compile_version": zlib.ZLIB_VERSION,
            "zlib_runtime_version": zlib.ZLIB_RUNTIME_VERSION,
            "go_sum_sha256": sha256(repository / "go.sum"),
            "build_flags": [
                "-buildvcs=true",
                "-trimpath",
                f"-ldflags=-s -w -X main.version={arguments.version}",
                "CGO_ENABLED=0",
                f"SOURCE_DATE_EPOCH={epoch}",
            ],
            "separate_build_caches": caches[0] != caches[1],
            "different_checkout_paths": checkouts[0] != checkouts[1],
        }
        print(json.dumps({"reproducibility_envelope": envelope}, indent=2, sort_keys=True))

        for index in range(2):
            worktrees.append(checkouts[index])
            build(
                repository,
                revision,
                arguments.version,
                epoch,
                checkouts[index],
                outputs[index],
                caches[index],
                temporary[index],
            )

        mismatches = compare(arguments.version, outputs[0], outputs[1])
        if mismatches:
            print("reproducibility verification failed:", file=sys.stderr)
            for mismatch in mismatches:
                print(f"  {mismatch}", file=sys.stderr)
            return 1
        print(f"reproducibility verification passed for revision {revision}: {len(expected_artifacts(arguments.version))} artifacts are byte-identical")
        return 0
    finally:
        for worktree in worktrees:
            subprocess.run(
                ["git", "worktree", "remove", "--force", str(worktree)],
                cwd=repository,
                check=False,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        subprocess.run(
            ["git", "worktree", "prune"],
            cwd=repository,
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
