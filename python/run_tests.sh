#!/usr/bin/env bash
# Every test file in its own process (Django is configured once per
# process), with the shared vectors and every framework REQUIRED: a missing
# vector file or framework is a failure, never a skip.
#
#   PYTHON=.venv/bin/python ./run_tests.sh
#
# Test-only dependencies: pip install django flask fastapi uvicorn celery
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
PYTHON="${PYTHON:-python3}"
export ADT_REQUIRE_VECTORS=1 DEVBENCH_REQUIRE_FRAMEWORKS=1 PYTHONDONTWRITEBYTECODE=1
for v in $(env | grep -E '^(DEVBENCH_DSN|ADT_DSN|DEVBENCH_SERVICE|DEVBENCH_RELEASE|DEVBENCH_ENABLED)=' | cut -d= -f1); do unset "$v"; done
"$PYTHON" -c 'import sys; print("python", sys.version.split()[0])'
for f in tests/test_*.py; do
  mod="tests.$(basename "$f" .py)"
  echo "== $mod"
  out="$("$PYTHON" -W error::DeprecationWarning:devbench -m unittest "$mod" 2>&1)" || { echo "$out"; exit 1; }
  echo "$out" | tail -n 3
  if echo "$out" | grep -qE 'skipped=[1-9]'; then echo "skipped tests in $mod"; exit 1; fi
done
echo "all Python SDK tests passed"
