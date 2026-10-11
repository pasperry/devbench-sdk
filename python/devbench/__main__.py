"""``python -m devbench test``: checks DEVBENCH_DSN against Dev Bench and
prints the HTTP result, without waiting for a flush. Exits non-zero when the
DSN is missing, malformed, unreachable or rejected."""

from __future__ import annotations

import sys
from typing import List, Optional

from . import _core

USAGE = "usage: python -m devbench test\n"


def main(argv: Optional[List[str]] = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if args != ["test"]:
        sys.stderr.write(USAGE)
        return 2
    return 0 if _core.self_test(sys.stdout) else 1


if __name__ == "__main__":
    sys.exit(main())
