"""Runs the mountable binary for this platform in place of this process."""

import os
from pathlib import Path
import platform
import signal
import subprocess
import sys

ARCHES = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}


def main() -> None:
    system = platform.system().lower()
    arch = ARCHES.get(platform.machine().lower())
    suffix = ".exe" if system == "windows" else ""
    binary = Path(__file__).parent / "bin" / f"mountable-{system}-{arch}{suffix}"
    if system not in ("linux", "darwin", "windows") or arch is None or not binary.is_file():
        sys.exit(
            f"mountable: {platform.system()}/{platform.machine()} is not supported by this package;"
            " mountable.io runs on Linux and macOS (x86_64, arm64)."
        )
    if system == "windows":
        # os.execv does not replace the process on Windows. Ctrl-C and
        # closing the console reach the binary directly, as it shares this
        # console; this process only waits for it and passes on its exit code.
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        sys.exit(subprocess.call([str(binary), *sys.argv[1:]]))
    if not os.access(binary, os.X_OK):
        # Installers that drop the wheel's file modes.
        binary.chmod(binary.stat().st_mode | 0o755)
    os.execv(binary, [str(binary), *sys.argv[1:]])
