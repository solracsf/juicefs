#!/bin/bash
# $1 = start|continue. Runs the reorder rebase of h0 onto 9325fe3, auto-resolving
# whitespace-only conflicts with resolve.sh and stopping on anything else
# (STOP-MANUAL) or on a ready carry (STOP-OTHER, the exec carry check failed).
if [ "$1" = start ]; then
  out=$(GIT_SEQUENCE_EDITOR="python ../seq.py" git rebase -i --autosquash 9325fe3 2>&1)
else
  out=$(GIT_EDITOR=true git rebase --continue 2>&1)
fi
while true; do
  echo "$out" | grep -E 'CONFLICT|Could not apply|Successfully|error|failed' | grep -v '^hint'
  [ -d .git/rebase-merge ] || break
  if git diff --name-only --diff-filter=U | grep -q .; then
    ../resolve.sh || { echo STOP-MANUAL; break; }
  else
    echo "$out" | tail -5; echo STOP-OTHER; break
  fi
  out=$(GIT_EDITOR=true git rebase --continue 2>&1)
done
