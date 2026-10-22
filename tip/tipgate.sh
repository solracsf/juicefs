#!/bin/bash
# Tip gate, run inside the snaptip container (golang:1.25, worktree at /w,
# network shared with redis, mysql:8.0, postgres:16 and etcd on 127.0.0.1:3379,
# ETCD_ADDR=127.0.0.1:3379). Prints one marker line per step.
cd /w
run(){ local name=$1; shift; local t=$(date +%s); echo "######## $name: $*"; bash -c "$*" > /tmp/$name.log 2>&1; local rc=$?; echo "######## $name rc=$rc $(( $(date +%s)-t ))s"; grep -aE '^(--- FAIL|FAIL|ok|panic:)' /tmp/$name.log | head -40; }
for i in $(seq 1 60); do (echo > /dev/tcp/127.0.0.1/3306) 2>/dev/null && break; sleep 2; done
run core     "go test ./pkg/meta/ -count=1 -timeout 60m -skip 'Etcd|TiKV|Tikv|PostgreSQL|MySQL|KeyDB|RedisCluster|LoadDumpSlow|FDB|^TestLoadDump\$'"
run loaddump "go test ./pkg/meta/ -count=1 -timeout 30m -run '^TestLoadDump\$/Metadata_Engine:_(redis|sqlite3|badger)\$' -v"
run mysql    "go test ./pkg/meta/ -count=1 -timeout 60m -run 'TestMySQLClient' -v"
run etcd     "go test ./pkg/meta/ -count=1 -timeout 60m -run '^(TestEtcdClient|TestEtcd)\$' -v"
run vfs      "go test ./pkg/vfs/ -count=1 -timeout 30m"
run fs       "go test ./pkg/fs/ -count=1 -timeout 30m"
run cmd      "go test ./cmd/ -count=1 -timeout 30m -run 'TestArgsOrder|TestFormatMaxSnapshots|TestFormatVersion|TestSnapshotSubcommandFlags' -v"
echo ALLDONE
