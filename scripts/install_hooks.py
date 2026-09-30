#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Install the pre-commit hook that runs scripts/check-launcher-files.py.

Deliberately a Python file rather than a .cmd full of `>> echo` lines: quoting
a shell script through cmd.exe is how the guard itself ends up broken.
"""

import os
import stat
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

HOOK_BODY = """#!/bin/sh
# Installed by scripts/install-hooks.cmd (via install_hooks.py).
# Rejects non-ASCII .vbs and non-CRLF launcher files, which are the two ways
# the windowless watchdog silently stops working.
set -e
if command -v python3 >/dev/null 2>&1; then
    PY=python3
elif command -v python >/dev/null 2>&1; then
    PY=python
else
    echo "pre-commit: python not found; skipping launcher file check" >&2
    exit 0
fi
exec "$PY" scripts/check-launcher-files.py
"""


def main():
    git_dir = os.path.join(ROOT, ".git")
    if not os.path.isdir(git_dir):
        print("[warn] %s is not a git checkout; skipping hook install" % ROOT)
        return 0

    hooks_dir = os.path.join(git_dir, "hooks")
    if not os.path.isdir(hooks_dir):
        os.makedirs(hooks_dir)

    hook_path = os.path.join(hooks_dir, "pre-commit")
    if os.path.exists(hook_path):
        with open(hook_path, "rb") as handle:
            existing = handle.read()
        if b"check-launcher-files.py" not in existing:
            backup = hook_path + ".bak"
            with open(backup, "wb") as handle:
                handle.write(existing)
            print("[info] existing pre-commit backed up to %s" % backup)

    with open(hook_path, "wb") as handle:
        handle.write(HOOK_BODY.replace("\n", "\n").encode("utf-8"))

    os.chmod(hook_path, os.stat(hook_path).st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    print("[ok] pre-commit hook installed at %s" % hook_path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
