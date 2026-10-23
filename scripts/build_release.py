#!/usr/bin/env python3
"""Build deterministic release archives with complete license texts."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import zipfile


ROOT = Path(__file__).resolve().parents[1]
COMMAND_PACKAGE = "./cmd/restic-defensive-mcp"
TARGETS = (
    ("linux", "amd64", "tar.gz"),
    ("linux", "arm64", "tar.gz"),
    ("darwin", "arm64", "tar.gz"),
    ("windows", "amd64", "zip"),
)
LICENSE_PREFIXES = ("LICENSE", "NOTICE", "COPYING", "PATENTS")


def run_command(arguments: list[str], env: dict[str, str] | None = None) -> str:
    """Run one command and return its standard output."""
    result = subprocess.run(
        arguments,
        cwd=ROOT,
        env=env,
        check=True,
        stdout=subprocess.PIPE,
        text=True,
    )
    return result.stdout


def decode_json_stream(content: str) -> list[dict[str, object]]:
    """Decode concatenated JSON objects from a Go command."""
    decoder = json.JSONDecoder()
    values: list[dict[str, object]] = []
    offset = 0
    while offset < len(content):
        while offset < len(content) and content[offset].isspace():
            offset += 1
        if offset == len(content):
            break
        value, offset = decoder.raw_decode(content, offset)
        if not isinstance(value, dict):
            raise ValueError("Go command returned a non-object JSON value")
        values.append(value)
    return values


def safe_module_destination(base: Path, module_path: str, version: str) -> Path:
    """Return a destination that cannot escape the license directory."""
    parts = module_path.split("/")
    if not parts or any(part in {"", ".", ".."} or "\\" in part for part in parts):
        raise ValueError(f"unsafe module path: {module_path!r}")
    return base.joinpath(*parts[:-1], f"{parts[-1]}@{version}")


def module_licenses(target_os: str, target_arch: str) -> list[tuple[str, str, Path]]:
    """List modules linked into one target binary."""
    env = os.environ.copy()
    env.update({"GOOS": target_os, "GOARCH": target_arch, "CGO_ENABLED": "0"})
    packages = decode_json_stream(run_command(["go", "list", "-deps", "-json", COMMAND_PACKAGE], env))
    modules: dict[tuple[str, str], Path] = {}
    for package in packages:
        module = package.get("Module")
        if not isinstance(module, dict) or module.get("Main") is True:
            continue
        if module.get("Replace") is not None:
            raise ValueError(f"replaced module requires manual license review: {module.get('Path')}")
        path = module.get("Path")
        version = module.get("Version")
        directory = module.get("Dir")
        if not all(isinstance(value, str) and value for value in (path, version, directory)):
            raise ValueError(f"incomplete module metadata: {module!r}")
        modules[(path, version)] = Path(directory)
    return [(path, version, modules[(path, version)]) for path, version in sorted(modules)]


def copy_file(source: Path, destination: Path, mode: int) -> None:
    """Copy a file without changing its content."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    destination.chmod(mode)


def copy_module_licenses(stage_dir: Path, target_os: str, target_arch: str) -> None:
    """Copy complete license files for linked modules."""
    license_root = stage_dir / "licenses"
    for module_path, version, module_dir in module_licenses(target_os, target_arch):
        sources = sorted(
            path
            for path in module_dir.iterdir()
            if path.is_file() and path.name.upper().startswith(LICENSE_PREFIXES)
        )
        if not sources:
            raise FileNotFoundError(f"no license file for {module_path}@{version}")
        destination = safe_module_destination(license_root, module_path, version)
        for source in sources:
            copy_file(source, destination / source.name, 0o644)


def copy_go_license(stage_dir: Path) -> None:
    """Copy the Go standard library license and patent grant."""
    go_env = json.loads(run_command(["go", "env", "-json", "GOROOT", "GOVERSION"]))
    goroot = Path(go_env["GOROOT"])
    version = go_env["GOVERSION"]
    destination = stage_dir / "licenses" / f"go-stdlib@{version}"

    license_candidates = (goroot / "LICENSE", goroot.parent / "LICENSE")
    license_source = next((path for path in license_candidates if path.is_file()), None)
    if license_source is None:
        raise FileNotFoundError(f"Go LICENSE not found near {goroot}")
    copy_file(license_source, destination / "LICENSE", 0o644)

    patents = goroot / "PATENTS"
    if patents.is_file():
        copy_file(patents, destination / "PATENTS", 0o644)


def set_staged_mtimes(stage_dir: Path, epoch: int) -> None:
    """Set one timestamp on all staged files and directories."""
    for path in sorted(stage_dir.rglob("*"), reverse=True):
        os.utime(path, (epoch, epoch), follow_symlinks=False)
    os.utime(stage_dir, (epoch, epoch), follow_symlinks=False)


def tar_info(path: Path, relative: Path, epoch: int) -> tarfile.TarInfo:
    """Create normalized tar metadata for one staged path."""
    name = relative.as_posix()
    if path.is_dir():
        info = tarfile.TarInfo(name + "/")
        info.type = tarfile.DIRTYPE
        info.mode = 0o755
    else:
        info = tarfile.TarInfo(name)
        info.size = path.stat().st_size
        info.mode = stat.S_IMODE(path.stat().st_mode)
    info.uid = 0
    info.gid = 0
    info.uname = ""
    info.gname = ""
    info.mtime = epoch
    return info


def write_tar_archive(stage_dir: Path, archive: Path, epoch: int) -> None:
    """Write a deterministic gzip-compressed tar archive."""
    paths = sorted(stage_dir.rglob("*"), key=lambda path: path.relative_to(stage_dir).as_posix())
    with archive.open("wb") as raw_file:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw_file, mtime=epoch, compresslevel=9) as gzip_file:
            with tarfile.open(fileobj=gzip_file, mode="w", format=tarfile.PAX_FORMAT) as tar_file:
                for path in paths:
                    relative = path.relative_to(stage_dir)
                    info = tar_info(path, relative, epoch)
                    if path.is_dir():
                        tar_file.addfile(info)
                    else:
                        with path.open("rb") as source:
                            tar_file.addfile(info, source)


def write_zip_archive(stage_dir: Path, archive: Path, epoch: int) -> None:
    """Write a deterministic zip archive with sorted files."""
    timestamp = max(epoch, 315532800)
    date_parts = list(time.gmtime(timestamp)[:6])
    date_parts[5] -= date_parts[5] % 2
    date_time = tuple(date_parts)
    files = sorted(
        (path for path in stage_dir.rglob("*") if path.is_file()),
        key=lambda path: path.relative_to(stage_dir).as_posix(),
    )
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as zip_file:
        for path in files:
            relative = path.relative_to(stage_dir).as_posix()
            info = zipfile.ZipInfo(relative, date_time=date_time)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = stat.S_IMODE(path.stat().st_mode) << 16
            zip_file.writestr(info, path.read_bytes(), compresslevel=9)


def build_target(version: str, target_os: str, target_arch: str, archive_type: str, output_dir: Path, epoch: int) -> Path:
    """Build one binary and its deterministic distribution archive."""
    extension = ".exe" if target_os == "windows" else ""
    archive_name = f"restic-defensive-mcp_{version}_{target_os}_{target_arch}.{archive_type}"
    archive = output_dir / archive_name
    if archive.exists():
        raise FileExistsError(f"release asset already exists: {archive}")

    with tempfile.TemporaryDirectory(prefix="restic-defensive-mcp-release-") as temporary:
        stage_dir = Path(temporary)
        binary = stage_dir / f"restic-defensive-mcp{extension}"
        env = os.environ.copy()
        env.update(
            {
                "CGO_ENABLED": "0",
                "GOARCH": target_arch,
                "GOOS": target_os,
                "SOURCE_DATE_EPOCH": str(epoch),
            }
        )
        subprocess.run(
            [
                "go",
                "build",
                "-buildvcs=true",
                "-trimpath",
                "-ldflags",
                f"-s -w -X main.version={version}",
                "-o",
                str(binary),
                COMMAND_PACKAGE,
            ],
            cwd=ROOT,
            env=env,
            check=True,
        )
        binary.chmod(0o755)
        copy_file(ROOT / "LICENSE", stage_dir / "LICENSE", 0o644)
        copy_file(ROOT / "THIRD_PARTY_NOTICES.md", stage_dir / "THIRD_PARTY_NOTICES.md", 0o644)
        copy_module_licenses(stage_dir, target_os, target_arch)
        copy_go_license(stage_dir)
        set_staged_mtimes(stage_dir, epoch)

        if archive_type == "zip":
            write_zip_archive(stage_dir, archive, epoch)
        else:
            write_tar_archive(stage_dir, archive, epoch)
    return archive


def write_checksums(archives: list[Path], output_dir: Path) -> Path:
    """Write sorted SHA-256 checksums for release archives."""
    checksum_file = output_dir / "SHA256SUMS"
    lines = []
    for archive in sorted(archives, key=lambda path: path.name):
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        lines.append(f"{digest}  {archive.name}\n")
    checksum_file.write_text("".join(lines), encoding="ascii", newline="\n")
    return checksum_file


def parse_arguments() -> argparse.Namespace:
    """Parse command-line arguments."""
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", required=True)
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    return parser.parse_args()


def main() -> int:
    """Build every supported release target."""
    arguments = parse_arguments()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", arguments.version):
        raise ValueError("version must use MAJOR.MINOR.PATCH")
    if arguments.source_date_epoch < 0:
        raise ValueError("source date epoch must not be negative")

    output_dir = arguments.output_dir.resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    if any(output_dir.iterdir()):
        raise FileExistsError(f"output directory must be empty: {output_dir}")
    archives = [
        build_target(arguments.version, target_os, target_arch, archive_type, output_dir, arguments.source_date_epoch)
        for target_os, target_arch, archive_type in TARGETS
    ]
    write_checksums(archives, output_dir)
    for archive in archives:
        print(archive)
    return 0


if __name__ == "__main__":
    sys.exit(main())
