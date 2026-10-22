#!/bin/bash
# FoundationDB leg: run in a throwaway golang:1.25 container with the worktree
# at /w. Installs Apple's official 6.3.23 client and server packages, starts
# fdbserver by hand, then runs the fdb-tagged meta tests.
set -e
cd /tmp
V=6.3.23
for p in clients server; do curl -fsSL -o fdb-$p.deb https://github.com/apple/foundationdb/releases/download/$V/foundationdb-${p}_$V-1_amd64.deb; done
ls -la fdb-*.deb
dpkg -i fdb-clients.deb >/dev/null
dpkg -i fdb-server.deb >/dev/null 2>&1 || true
mkdir -p /var/fdb/data /var/fdb/logs
cat /etc/foundationdb/fdb.cluster
nohup /usr/sbin/fdbserver -C /etc/foundationdb/fdb.cluster -p auto:4500 -d /var/fdb/data -L /var/fdb/logs > /var/fdb/server.out 2>&1 &
sleep 3
fdbcli --exec 'configure new single ssd' --timeout 30 || fdbcli --exec 'status minimal' --timeout 30
fdbcli --exec 'status minimal' --timeout 30
cd /w
go test -tags fdb ./pkg/meta/ -count=1 -timeout 60m -run '^(TestFdbClient|TestFdb)$' -v 2>&1 | grep -E '^(=== RUN   Test|--- FAIL|--- PASS: TestFdb|FAIL|ok|panic)' | head -60
