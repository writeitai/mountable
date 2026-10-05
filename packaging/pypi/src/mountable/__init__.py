"""Runs the mountable binary for this platform in place of this process."""

import os
from pathlib import Path
import platform
import sys

ARCHES = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}


def main() -> None:
    system = platform.system().lower()
    arch = ARCHES.get(platform.machine().lower())
    binary = Path(__file__).parent / "bin" / f"mountable-{system}-{arch}"
    if system not in ("linux", "darwin") or arch is None or not binary.is_file():
        sys.exit(
            f"mountable: {platform.system()}/{platform.machine()} is not supported;"
            " Mountable runs on Linux and macOS (x86_64, arm64)."
        )
    if not os.access(binary, os.X_OK):
        # Installers that drop the wheel's file modes.
        binary.chmod(binary.stat().st_mode | 0o755)
    os.execv(binary, [str(binary), *sys.argv[1:]])
