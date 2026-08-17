import os
import shutil
import subprocess
import sys
from pathlib import Path

def tncd_command():
    """Argv prefix for launching tncd (black-box e2e harness).

    Priority: $TNCD_BIN (may be multi-word), then a `tncd` binary on PATH or
    the repo-root build output, otherwise build the Go binary from ./cmd/tncd
    on demand into the repo root.
    """
    env = os.environ.get("TNCD_BIN")
    if env:
        return env.split()
    root = Path(__file__).parent.parent
    for candidate in (shutil.which("tncd"), root / "tncd"):
        if candidate and Path(candidate).is_file():
            return [str(candidate)]
    # No prebuilt binary — build the pure-Go binary into the repo root.
    out = root / "tncd"
    subprocess.run(
        ["go", "build", "-o", str(out), "./cmd/tncd"],
        cwd=str(root), check=True,
        env={**os.environ, "CGO_ENABLED": "0"},
    )
    return [str(out)]
