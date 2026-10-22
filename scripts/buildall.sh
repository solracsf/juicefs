#!/bin/bash
# Build + vet every commit of a range, inside golang:1.25 with the work clone
# mounted read-only at /src. usage (in the container): bash /buildall.sh 9325fe3..new2
git config --global --add safe.directory '*'
rm -rf /work && git clone -q /src /work && cd /work
export GOFLAGS=-buildvcs=false
for c in $(git rev-list --reverse $1); do
  git checkout -q $c
  s=$(git log -1 --format=%s)
  if out=$(go build ./... 2>&1 && go vet ./pkg/... ./cmd 2>&1); then echo "OK   $s"; else echo "FAIL $s"; echo "$out" | grep -v '^#' | head -8; fi
done
