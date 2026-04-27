import sys
from pathlib import Path

# Ensure the project root is on sys.path so agwkiss is importable without
# installation when running pytest from any directory.
sys.path.insert(0, str(Path(__file__).parent))
