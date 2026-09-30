#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Guard the windowless launcher files against the two failure modes that make
the watchdog either flash a window or die silently:

  1. keepalive.vbs must be pure ASCII. wscript.exe reads .vbs using the ANSI
     code page of the machine, so any non-ASCII byte gets mangled on a host
     with a different locale, and a mangled script means no watchdog at all.
  2. keepalive.vbs and the task XML must use CRLF. cmd.exe and Task Scheduler
     are far happier with CRLF, and a mixed-EOL file is a classic "works on my
     machine" bug.

Runs standalone (prints findings, exits 1 on failure) and from a pre-commit
hook. Invoked by scripts/install-keepalive.cmd in 'full' mode.
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

TARGETS = [
    ("ascii+crlf", os.path.join(HERE, "keepalive.vbs")),
    ("crlf",       os.path.join(HERE, "free2api-keepalive.xml")),
    ("ascii+crlf", os.path.join(HERE, "install-keepalive.cmd")),
    ("ascii+crlf", os.path.join(HERE, "register-keepalive-task.ps1")),
]


def check(path, want_ascii):
    problems = []
    if not os.path.exists(path):
        problems.append("missing")
        return problems
    with open(path, "rb") as handle:
        raw = handle.read()
    if b"\x00" in raw:
        problems.append("contains NUL bytes (probably UTF-16)")
    if want_ascii and any(byte > 0x7F for byte in raw):
        problems.append("contains non-ASCII bytes (wscript would mangle it)")
    if raw.count(b"\r\n") != raw.count(b"\n"):
        problems.append("uses bare-LF line endings (expected CRLF)")
    if raw.startswith(b"\xef\xbb\xbf"):
        problems.append("starts with a UTF-8 BOM")
    return problems


def main():
    failed = False
    for mode, path in TARGETS:
        want_ascii = "ascii" in mode
        problems = check(path, want_ascii)
        name = os.path.relpath(path, ROOT)
        if problems:
            failed = True
            print("FAIL %s: %s" % (name, "; ".join(problems)))
        else:
            print("ok   %s" % name)
    if failed:
        print("\nFix the launcher files before committing.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
