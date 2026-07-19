import os
import shutil
import sys
from pathlib import Path

def tncd_command():
    """Argv prefix for launching tncd, packet-browser style.

    Priority: $TNCD_BIN (may be multi-word, e.g. "python tncd.py"),
    then a Go binary on PATH or in the repo root, then the Python
    reference implementation.
    """
    env = os.environ.get("TNCD_BIN")
    if env:
        return env.split()
    root = Path(__file__).parent.parent
    for candidate in (shutil.which("tncd"), root / "tncd"):
        if candidate and Path(candidate).is_file():
            return [str(candidate)]
    return [sys.executable, str(root / "tncd.py")]
