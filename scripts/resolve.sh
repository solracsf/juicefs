#!/bin/bash
# All-or-nothing resolution of conflicted .go files: start from ours and apply
# the incoming commit's whitespace-insensitive diff; succeeds only when every
# hunk applies, then gofmt re-indents. Handles the conflicts caused by the
# Redis clone fix (fix 4) wrapping doCloneEntry/doBatchClone in a retry loop.
# Anything else is left conflicted for manual resolution.
set -u
R=$(git rev-parse REBASE_HEAD)
rc=0
for f in $(git diff --name-only --diff-filter=U); do
  case "$f" in *.go) ;; *) echo "MANUAL(non-go): $f"; rc=1; continue;; esac
  git checkout --ours -- "$f"
  git diff -w "$R^" "$R" -- "$f" > /tmp/res.diff
  if git apply --ignore-whitespace /tmp/res.diff 2>/tmp/res.err; then
    gofmt -w "$f"
    a=$(grep '^[-+]' /tmp/res.diff | grep -v '^[-+][-+]' | tr -d ' \t' | md5sum)
    b=$(git diff -w HEAD -- "$f" | grep '^[-+]' | grep -v '^[-+][-+]' | tr -d ' \t' | md5sum)
    if [ "$a" = "$b" ]; then echo "OK: $f"; git add "$f"; else echo "DIFFERS: $f"; rc=1; fi
  else
    git checkout -m -- "$f"; echo "APPLY-FAILED: $f"; head -5 /tmp/res.err; rc=1
  fi
done
exit $rc
