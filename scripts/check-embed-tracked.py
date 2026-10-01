# -*- coding: utf-8 -*-
"""Guard every go:embed target is actually tracked by git.

Why this exists: internal/prompt/defaultprompt.md is pulled into the binary by
`//go:embed defaultprompt.md`, but the repo-wide `*.md` rule in .gitignore kept
it out of version control. On the maintainer's machine the working-tree copy
made every build succeed, so nothing looked wrong -- but a fresh `git clone`
(CI, Docker build, anyone else) has no such file and fails hard with:

    internal/prompt/prompt.go:17:12: pattern defaultprompt.md: no matching files found

That silently broke the scheduled Build & Publish workflow for days. A build
that only passes because of untracked local files is worse than a failing one,
so this guard is deliberately loud about it.

Runs standalone (prints findings, exits 1 on failure) and is safe to call from
a pre-commit hook.
"""

import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# //go:embed 后面可以跟多个模式，也能带引号；这里只取同一行剩余部分。
EMBED_RE = re.compile(r"^\s*//go:embed\s+(.+?)\s*$")


def tracked_files():
    out = subprocess.run(
        ["git", "ls-files", "-z"],
        cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
    )
    if out.returncode != 0:
        return None
    return set(p.decode("utf-8", "replace") for p in out.stdout.split(b"\0") if p)


def walk_go_files():
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in (".git", "node_modules")]
        for fn in filenames:
            if fn.endswith(".go"):
                yield os.path.join(dirpath, fn)


def embed_targets(go_file):
    """Yield repo-relative paths that a //go:embed directive in go_file needs."""
    rel_dir = os.path.dirname(os.path.relpath(go_file, ROOT))
    with open(go_file, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = EMBED_RE.match(line)
            if not m:
                continue
            for raw in m.group(1).split():
                pat = raw.strip('"').strip("`")
                # 只处理字面路径（含通配）。变量拼接 / 目录形式这里不展开。
                if pat.startswith("/") or pat.startswith(".."):
                    continue
                yield os.path.normpath(os.path.join(rel_dir, pat)).replace("\\", "/")


def main():
    tracked = tracked_files()
    if tracked is None:
        print("check-embed-tracked: not a git checkout; skipping")
        return 0

    problems = []
    for go_file in walk_go_files():
        for target in embed_targets(go_file):
            if "*" in target or "?" in target:
                # 通配：至少要有一个匹配的文件被跟踪
                prefix_dir = os.path.dirname(target)
                if not any(t.startswith(prefix_dir + "/") for t in tracked):
                    problems.append((os.path.relpath(go_file, ROOT), target, "no tracked match"))
                continue
            if target not in tracked:
                problems.append((os.path.relpath(go_file, ROOT), target, "not tracked by git"))

    if problems:
        print("check-embed-tracked: FAIL")
        for go_file, target, why in problems:
            print("  %s embeds %s -> %s" % (go_file, target, why))
        print("")
        print("  A fresh clone would fail to build. Fix .gitignore so the file is")
        print("  tracked, then `git add -f <path>` if it was already committed once.")
        return 1

    print("check-embed-tracked: OK (%d go files scanned)" % sum(1 for _ in walk_go_files()))
    return 0


if __name__ == "__main__":
    sys.exit(main())