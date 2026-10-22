#!/bin/bash
# Multiset of changed lines: the incoming commit's own diff vs what the resolved
# file changes against HEAD. Empty output = the resolution applied exactly the
# commit's change (placement may differ). Use after every manual resolution.
f=$1
diff <(git diff REBASE_HEAD^ REBASE_HEAD -- $f | grep '^[-+]' | grep -v '^[-+][-+][-+] ' | sort) <(git diff HEAD -- $f | grep '^[-+]' | grep -v '^[-+][-+][-+] ' | sort)
