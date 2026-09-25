#!/bin/bash -e
# Battle test for snapshots: several clients writing while snapshots are taken,
# gc and compaction running underneath, processes killed or frozen, the metadata
# engine stalled or cut off. The oracle is snapshot_workload.py: every snapshot
# is checked against what the writers journaled, and re-read later from another
# client to prove it never changed.
source .github/scripts/common/common.sh

[[ -z "$META" ]] && META=redis
source .github/scripts/start_meta_engine.sh
start_meta_engine $META
META_URL=$(get_meta_url $META)

WL="python3 .github/scripts/command/snapshot_workload.py"
T=/tmp/snapbt
MOUNT_OPTS="--enable-xattr --atime-mode relatime"

fail(){
    echo "<FATAL>: $*"
    exit 1
}

mount_client(){
    local mp=$1
    ./juicefs mount -d $META_URL $mp $MOUNT_OPTS --cache-dir /var/jfsCache-$(basename $mp)
    wait_serving $mp
}

wait_serving(){
    for i in $(seq 1 30); do
        stat $1/.accesslog > /dev/null 2>&1 && return 0
        sleep 1
    done
    fail "$1 is not serving"
}

# the names of the snapshots, without the header and the total line
snapshots(){
    ./juicefs snapshot list $META_URL 2>/dev/null | awk 'NR > 1 && $1 !~ /^\(/ { print $1 }'
}

# every process of the mount at $1: with -d, a supervisor and the one serving it
mount_pids(){
    ps -eo pid=,args= | awk -v mp="$1" '/juicefs/ && / mount / { for (i = 2; i <= NF; i++) if ($i == mp) print $1 }'
}

start_volume(){
    prepare_test
    rm -rf /var/jfsCache-jfs* $T
    mkdir -p $T
    ./juicefs format $META_URL myjfs --trash-days 0
    mount_client /jfs
}

# snap NAME [ARGS]: create a snapshot of /bt, recording when the call started and ended
snap(){
    local name=$1
    shift
    START=$(date +%s%N)
    ./juicefs snapshot create $META_URL --path ${SNAP_PATH:-/bt} --name $name "$@"
    local rc=$?
    END=$(date +%s%N)
    return $rc
}

listed(){
    snapshots | grep -qx "$1"
}

# gone PATH: true once PATH has disappeared from a mount, whose kernel may keep a
# removed entry for the length of its entry cache (1s by default)
gone(){
    for i in $(seq 1 10); do
        [[ -e $1 ]] || return 0
        sleep 0.5
    done
    return 1
}

objects(){
    find /var/jfs/myjfs/chunks -type f 2>/dev/null | wc -l
}

# make_tree ROOT [FILES]: 20 directories of FILES (40 by default) files each
make_tree(){
    local root=$1
    mkdir -p $root
    python3 - "$root" "${2:-40}" <<'EOF'
import os, sys
root = sys.argv[1]
for d in range(20):
    os.makedirs(f"{root}/d{d}/sub", exist_ok=True)
    for f in range(int(sys.argv[2])):
        with open(f"{root}/d{d}/f{f}", "wb") as fh:
            fh.write(os.urandom(4096 * (1 + f % 3)))
    os.setxattr(f"{root}/d{d}/f0", "user.tag", b"x")
    os.link(f"{root}/d{d}/f1", f"{root}/d{d}/sub/hard")
    os.symlink("../f2", f"{root}/d{d}/sub/soft")
    os.makedirs(f"{root}/d{d}/empty", exist_ok=True)
EOF
}

# Reads through a mount leave a snapshot alone, and nothing in it can be changed
# through the mount, whatever the operation.
test_snapshot_readonly(){
    start_volume
    make_tree /jfs/bt
    snap s1
    $WL manifest /jfs/.snapshots/s1 $T/m1
    local s=/jfs/.snapshots/s1/d0
    cat $s/f0 $s/sub/hard > /dev/null
    ls -lR /jfs/.snapshots/s1 > /dev/null
    $WL manifest /jfs/.snapshots/s1 $T/m2
    $WL compare $T/m1 $T/m2 || fail "reading a snapshot changed it"
    for op in "touch $s/new" "sh -c 'echo x >> $s/f0'" "truncate -s 0 $s/f0" "rm $s/f0" \
              "mv $s/f0 $s/f00" "mv $s/f0 /jfs/escaped" "chmod 777 $s/f0" "chown 1234 $s/f0" \
              "setfattr -n user.k -v v $s/f0" "setfattr -x user.tag $s/f0" "ln $s/f0 $s/link" \
              "ln $s/f0 /jfs/outside" "mkdir $s/newdir" "rmdir $s/empty" "ln -s x $s/sym" "touch -d 2001-01-01 $s/f0" \
              "rm -rf /jfs/.snapshots/s1" "mv /jfs/.snapshots/s1 /jfs/stolen" "mkdir /jfs/.snapshots/forged"; do
        if eval "$op" 2>/dev/null; then
            fail "'$op' succeeded on a snapshot"
        fi
    done
    $WL manifest /jfs/.snapshots/s1 $T/m3
    $WL compare $T/m1 $T/m3 || fail "a refused write still changed the snapshot"
    # a copy made with clone is an ordinary, writable file
    ./juicefs clone /jfs/.snapshots/s1/d0/f0 /jfs/restored
    echo more >> /jfs/restored
    cmp -n 4096 /jfs/restored /jfs/.snapshots/s1/d0/f0 || fail "clone of a snapshot file differs"
}

# Two clients write, a third reads; consistent snapshots are taken, with best-effort
# ones where the tree never holds still, while compaction and gc run. Each is
# checked against the journals, then re-read from a fresh client after the live
# tree is gone, then deleted, which must give every object back.
test_snapshot_under_load(){
    start_volume
    mount_client /jfs2
    mount_client /jfs3
    $WL setup /jfs/bt 2
    rm -f $T/j0 $T/j1 $T/pause
    SEED=$RANDOM $WL write /jfs/bt 0 $T/j0 900 &
    local w0=$!
    SEED=$RANDOM $WL write /jfs2/bt 1 $T/j1 900 &
    local w1=$!
    (
        for i in $(seq 1 8); do
            ./juicefs compact /jfs/bt >/dev/null 2>&1 || true
            ./juicefs gc $META_URL --compact --delete >/dev/null 2>&1 || true
            sleep 2
        done
    ) &
    local maint=$!
    local prev="" consistent=0 besteffort=0 names=""
    for i in $(seq 1 12); do
        sleep 1
        # every other round the writers pause, so a consistent snapshot must succeed
        (( i % 2 == 0 )) && touch $T/pause && sleep 0.3
        local mode=""
        if snap s$i; then
            mode=--atomic
            consistent=$((consistent + 1))
        else
            listed s$i && fail "a failed create left s$i listed"
            [[ -e /jfs3/.snapshots/s$i ]] && fail "a failed create left s$i visible"
            (( i % 2 == 0 )) && fail "a consistent snapshot of a quiet tree failed"
            snap s$i --best-effort || fail "best-effort snapshot s$i failed"
            besteffort=$((besteffort + 1))
        fi
        rm -f $T/pause
        echo "$START $END $mode" > $T/s$i.when
        $WL verify /jfs3/.snapshots/s$i $T/j0 $T/j1 --start $START --end $END $mode \
            ${prev:+--prev $prev} --gens $T/s$i.gens || fail "snapshot s$i is wrong"
        $WL manifest /jfs3/.snapshots/s$i $T/s$i.m1
        prev=$T/s$i.gens
        names="$names s$i"
    done
    kill -TERM $w0 $w1 || fail "a writer died before the end"
    wait $w0 $w1 || true
    wait $maint || true
    echo "consistent snapshots: $consistent, best-effort: $besteffort"
    (( consistent >= 6 )) || fail "too few consistent snapshots"

    # the live tree goes away and everything is compacted and collected
    rm -rf /jfs/bt
    ./juicefs gc $META_URL --compact --delete
    umount_jfs /jfs2 $META_URL
    umount_jfs /jfs3 $META_URL
    rm -rf /var/jfsCache-jfs2
    mount_client /jfs2
    for s in $names; do
        $WL manifest /jfs2/.snapshots/$s $T/$s.m2
        $WL compare $T/$s.m1 $T/$s.m2 || fail "snapshot $s changed"
        read start end mode < $T/$s.when
        $WL verify /jfs2/.snapshots/$s $T/j0 $T/j1 --start $start --end $end $mode || fail "snapshot $s decayed"
    done
    for s in $names; do
        ./juicefs snapshot delete $META_URL --name $s
    done
    ./juicefs gc $META_URL --delete
    [[ $(objects) -eq 0 ]] || fail "$(objects) objects left after every file and snapshot is gone"
}

# Changes undone over and over (a file moved round three directories, counters that cycle
# in contents and in extended attributes) while snapshots are taken. A consistent
# snapshot must never hold a state the tree never had, even one whose parts all
# look unchanged when checked one at a time; best-effort snapshots must show such
# states at least once, or the adversary proved nothing.
test_snapshot_never_existing(){
    start_volume
    mount_client /jfs2
    $WL aba-setup /jfs/aba
    CONSISTENT=0 BUSY=0 TORN=0
    # all adversaries together, then extended attributes alone: their changes are
    # the only ones a comparison of values could miss if they left ctime alone
    local pids
    SEED=$RANDOM $WL aba-run /jfs/aba rename 60 & pids="$!"
    SEED=$RANDOM $WL aba-run /jfs2/aba pairs 60 & pids="$pids $!"
    SEED=$RANDOM $WL aba-run /jfs2/aba xattrs 60 & pids="$pids $!"
    aba_rounds a 55
    wait $pids || true
    SEED=$RANDOM $WL aba-run /jfs2/aba xattrs 60 & pids="$!"
    aba_rounds x 55
    wait $pids || true
    echo "consistent: $CONSISTENT, refused as busy: $BUSY, best-effort copies holding a state that never existed: $TORN"
    [[ $TORN -gt 0 ]] || fail "no best-effort copy caught a state that never existed, the adversary proved nothing"
    [[ $CONSISTENT -gt 0 ]] || fail "no consistent snapshot succeeded, nothing was checked"
}

# aba_rounds PREFIX SECONDS: consistent and best-effort snapshots of /aba, in turns
aba_rounds(){
    local end=$((SECONDS + $2)) i=0
    while [[ $SECONDS -lt $end ]]; do
        i=$((i + 1))
        if SNAP_PATH=/aba snap $1c$i > /dev/null 2>&1; then
            CONSISTENT=$((CONSISTENT + 1))
            $WL aba-check /jfs/.snapshots/$1c$i || fail "consistent snapshot $1c$i holds a state that never existed"
            ./juicefs snapshot delete $META_URL --name $1c$i > /dev/null
        else
            listed $1c$i && fail "a failed create left $1c$i listed"
            BUSY=$((BUSY + 1))
        fi
        SNAP_PATH=/aba snap $1b$i --best-effort > /dev/null 2>&1 || fail "best-effort snapshot $1b$i failed"
        if ! $WL aba-check /jfs/.snapshots/$1b$i > $T/aba.out; then
            TORN=$((TORN + 1))
            grep "forbidden" $T/aba.out | sed "s/^NEVER EXISTED: /best-effort $1b$i: /"
        fi
        ./juicefs snapshot delete $META_URL --name $1b$i > /dev/null
    done
}

# Holds keep a snapshot from being deleted, each tag on its own, through a client
# crash and gc; they cannot be forged or cleared as extended attributes; and a
# hold racing a delete never loses.
test_snapshot_holds(){
    start_volume
    mount_client /jfs2
    make_tree /jfs/tree
    setfattr -n user.k -v v /jfs/tree
    export SNAP_PATH=/tree
    snap s
    $WL manifest /jfs/.snapshots/s $T/s
    # a hold is not snapshot content: the root reads the same held or not
    root_view(){ stat -c '%Z %Y %X %a %u %g %s' /jfs/.snapshots/s; getfattr -d -m - /jfs/.snapshots/s; }
    root_view > $T/root.unheld
    grep -q user.k $T/root.unheld || fail "the snapshot root lost its attribute"
    ./juicefs snapshot hold $META_URL --name s --tag backup-123
    ./juicefs snapshot hold $META_URL --name s --tag replica
    root_view | diff $T/root.unheld - || fail "a hold changed what the snapshot root shows"
    getfattr -n juicefs.snapshot.hold.replica /jfs/.snapshots/s 2>/dev/null && fail "a hold reads as an attribute"
    ./juicefs snapshot hold $META_URL --name s --tag "$(printf 't%.0s' {1..201})" 2>/dev/null && fail "a 201-byte tag was taken"
    ./juicefs snapshot hold $META_URL --name s --tag "$(printf 'a\tb')" 2>/dev/null && fail "a tag with a control character was taken"
    ./juicefs snapshot hold $META_URL --name s --tag "sauvegarde-été/Ωμέγα:1" || fail "a unicode tag was refused"
    ./juicefs snapshot release $META_URL --name s --tag "sauvegarde-été/Ωμέγα:1" || fail "a unicode tag was not released"
    ./juicefs snapshot list $META_URL | grep -w s | grep -q "backup-123,replica" || fail "list does not show both holds"
    ./juicefs snapshot hold $META_URL --name s --tag replica 2>/dev/null && fail "a duplicate hold succeeded"
    ./juicefs snapshot release $META_URL --name s --tag nobody 2>/dev/null && fail "releasing a hold never taken succeeded"
    ./juicefs snapshot delete $META_URL --name s 2> $T/del.err && fail "a held snapshot was deleted"
    grep -q "is held" $T/del.err || fail "the delete error does not say the snapshot is held"
    # a hold is metadata: a client crash and gc leave it and the snapshot alone
    kill -9 $(mount_pids /jfs2)
    umount -l /jfs2 || true
    ./juicefs gc $META_URL --delete
    listed s || fail "a held snapshot vanished"
    $WL manifest /jfs/.snapshots/s $T/s2
    $WL compare $T/s $T/s2 || fail "a held snapshot changed"
    # it is not an attribute anyone can set or clear through a mount
    setfattr -n juicefs.snapshot.hold.forged -v x /jfs/tree/d0/f0 2>/dev/null && fail "a hold attribute was forged on a live file"
    setfattr -x juicefs.snapshot.hold.replica /jfs/.snapshots/s 2>/dev/null && fail "a hold was cleared as an attribute"
    ./juicefs snapshot release $META_URL --name s --tag backup-123
    ./juicefs snapshot delete $META_URL --name s 2>/dev/null && fail "a snapshot with a hold left was deleted"
    ./juicefs snapshot release $META_URL --name s --tag replica
    root_view | diff $T/root.unheld - || fail "releasing its holds changed what the snapshot root shows"
    ./juicefs snapshot delete $META_URL --name s || fail "delete after every release failed"

    # a hold and a delete at once: never both
    for i in $(seq 1 10); do
        snap r$i > /dev/null 2>&1
        ./juicefs snapshot hold $META_URL --name r$i --tag racer > /dev/null 2>&1 & local h=$!
        ./juicefs snapshot delete $META_URL --name r$i > /dev/null 2>&1 & local d=$!
        local held=0 deleted=0
        wait $h && held=1 || true
        wait $d && deleted=1 || true
        [[ $held -eq 1 && $deleted -eq 1 ]] && fail "round $i: both the hold and the delete succeeded"
        [[ $held -eq 0 && $deleted -eq 0 ]] && fail "round $i: neither the hold nor the delete succeeded"
        if [[ $held -eq 1 ]]; then
            listed r$i || fail "round $i: a held snapshot is gone"
            ./juicefs snapshot release $META_URL --name r$i --tag racer
            ./juicefs snapshot delete $META_URL --name r$i
        fi
    done
    unset SNAP_PATH
}

# A create killed at any point leaves no snapshot on view, and gc reaps what it
# left behind once it is old enough; the live tree never notices.
test_snapshot_kill_create(){
    start_volume
    make_tree /jfs/tree
    $WL manifest /jfs/tree $T/live
    export SNAP_PATH=/tree
    local killed=0
    for delay in 0.05 0.1 0.2 0.3 0.6 1 2; do
        ./juicefs snapshot create $META_URL --path /tree --name k &
        local pid=$!
        sleep $delay
        kill -9 $pid 2>/dev/null || true
        wait $pid || true
        if listed k; then
            $WL manifest /jfs/.snapshots/k $T/k
            $WL compare $T/live $T/k --ignore atime || fail "a create that finished before its kill is wrong"
            ./juicefs snapshot delete $META_URL --name k
            # or the next k could be read through the entry of this one
            gone /jfs/.snapshots/k || fail "a deleted snapshot is still visible"
        else
            gone /jfs/.snapshots/k || fail "a killed create is visible"
            killed=$((killed + 1))
        fi
    done
    echo "$killed creates were killed before they finished"
    [[ $killed -gt 0 ]] || fail "no create was killed mid-way, the test proved nothing"
    if [[ "$META" == "redis" ]]; then
        # age what the killed builds left, as a day would, and let gc reap it
        local db=${META_URL##*/}; db=${db%%\?*}
        local left=$(redis-cli -n $db zcard detachedNodes)
        echo "killed builds left $left detached trees"
        [[ $left -gt 0 ]] || fail "killed creates left nothing behind to reap, the test proved nothing"
        for ino in $(redis-cli -n $db zrange detachedNodes 0 -1); do
            redis-cli -n $db zadd detachedNodes 1 $ino > /dev/null
        done
        ./juicefs gc $META_URL --delete
        [[ $(redis-cli -n $db zcard detachedNodes) -eq 0 ]] || fail "gc left killed builds behind"
    else
        ./juicefs gc $META_URL --delete
    fi
    $WL manifest /jfs/tree $T/live2
    $WL compare $T/live $T/live2 --ignore atime || fail "reaping killed builds touched the live tree"
    snap k || fail "create after the kills failed"
    $WL manifest /jfs/.snapshots/k $T/k
    $WL compare $T/live $T/k --ignore atime || fail "snapshot after the kills is wrong"
    unset SNAP_PATH
}

# A delete killed at any point either did nothing or hid the snapshot for good;
# gc finishes the job, and the objects only the snapshot held are released.
test_snapshot_kill_delete(){
    start_volume
    make_tree /jfs/tree
    $WL manifest /jfs/tree $T/live
    export SNAP_PATH=/tree
    local db=${META_URL##*/}; db=${db%%\?*}
    local interrupted=0
    for delay in 0.02 0.05 0.1 0.2 0.5; do
        snap d
        ./juicefs snapshot delete $META_URL --name d &
        local pid=$!
        sleep $delay
        kill -9 $pid 2>/dev/null || true
        wait $pid || true
        if listed d; then
            $WL manifest /jfs/.snapshots/d $T/d
            $WL compare $T/live $T/d --ignore atime || fail "a snapshot survived a killed delete damaged"
            ./juicefs snapshot delete $META_URL --name d
        fi
        gone /jfs/.snapshots/d || fail "a deleted snapshot is still visible"
        # a delete killed after hiding the snapshot leaves its tree for gc
        if [[ "$META" == "redis" && $(redis-cli -n $db zcard detachedNodes) -gt 0 ]]; then
            interrupted=$((interrupted + 1))
        fi
        ./juicefs gc $META_URL --delete
        if [[ "$META" == "redis" && $(redis-cli -n $db zcard detachedNodes) -gt 0 ]]; then
            fail "gc left the tree of an interrupted delete"
        fi
    done
    $WL manifest /jfs/tree $T/live2
    echo "$interrupted deletes were killed half way"
    [[ "$META" != "redis" || $interrupted -gt 0 ]] || fail "no delete was killed half way, the test proved nothing"
    $WL compare $T/live $T/live2 --ignore atime || fail "killed deletes touched the live tree"
    rm -rf /jfs/tree
    ./juicefs gc $META_URL --delete
    [[ $(objects) -eq 0 ]] || fail "$(objects) objects leaked by killed deletes"
    unset SNAP_PATH
}

# A client killed mid-write, or frozen with writes in flight, does not stop
# snapshots, and what it committed before dying is in them.
test_snapshot_client_faults(){
    start_volume
    mount_client /jfs2
    $WL setup /jfs/bt 2
    rm -f $T/j0 $T/j1 $T/pause
    SEED=$RANDOM $WL write /jfs/bt 0 $T/j0 600 &
    local w0=$!
    SEED=$RANDOM $WL write /jfs2/bt 1 $T/j1 600 &
    local w1=$!
    sleep 3
    local pids=$(mount_pids /jfs2)
    [[ -n "$pids" ]] || fail "no process serves /jfs2"
    kill -9 $pids
    sleep 1
    [[ -z "$(mount_pids /jfs2)" ]] || fail "the /jfs2 client survived its kill"
    wait $w1 && fail "the writer on a killed client carried on" || true
    umount -l /jfs2 || true
    touch $T/pause && sleep 0.3
    snap crashed || fail "snapshot after a client crash failed"
    rm -f $T/pause
    $WL verify /jfs/.snapshots/crashed $T/j0 $T/j1 --start $START --end $END --atomic || fail "snapshot after a crash is wrong"

    mount_client /jfs2
    SEED=$RANDOM $WL write /jfs2/bt 1 $T/j1 600 &
    w1=$!
    sleep 2
    local pids=$(mount_pids /jfs2)
    [[ -n "$pids" ]] || fail "no process serves /jfs2"
    kill -STOP $pids
    for pid in $pids; do
        [[ $(ps -o stat= -p $pid) == T* ]] || fail "the /jfs2 client was not frozen"
    done
    touch $T/pause && sleep 0.3
    START=$(date +%s%N)
    timeout 120 ./juicefs snapshot create $META_URL --path /bt --name frozen || fail "a frozen client blocked a snapshot"
    END=$(date +%s%N)
    kill -CONT $pids
    rm -f $T/pause
    listed frozen || fail "snapshot with a frozen client is missing"
    kill -TERM $w0 $w1
    wait $w0 $w1 || true
    $WL verify /jfs/.snapshots/frozen $T/j0 $T/j1 --start $START --end $END --atomic || fail "snapshot with a frozen client is wrong"
    stat /jfs2/.accesslog > /dev/null || fail "the frozen client did not recover"
}

# The metadata engine stalls or drops off the network while a snapshot is taken:
# the create either completes with a correct snapshot or fails leaving none. Each
# fault is injected once the create has started copying, which it shows by
# registering the root it builds as a detached node, and each create has to be
# held up by its fault, or the test proved nothing.
test_snapshot_meta_faults(){
    [[ "$META" != "redis" ]] && echo "meta faults are injected into redis only, skipped" && return 0
    start_volume
    # large enough for the copy to still be running when the fault lands
    make_tree /jfs/tree 200
    $WL manifest /jfs/tree $T/live
    local db=${META_URL##*/}; db=${db%%\?*}
    local n=0 faults="pause busy"
    command -v iptables > /dev/null && faults="$faults partition"
    for fault in $faults; do
        n=$((n + 1))
        # a create that failed under the previous fault may have left its tree for gc
        local before=$(redis-cli -n $db zcard detachedNodes)
        (
            local t0=$(date +%s%N) rc=0
            timeout 300 ./juicefs snapshot create $META_URL --path /tree --name f$n > $T/f$n.log 2>&1 || rc=$?
            echo "$rc $(( ($(date +%s%N) - t0) / 1000000 ))" > $T/f$n.rc
        ) &
        local job=$!
        local started=0
        for i in $(seq 1 200); do
            [[ $(redis-cli -n $db zcard detachedNodes) -gt $before ]] && started=1 && break
            kill -0 $job 2>/dev/null || break
            sleep 0.05
        done
        [[ $started -eq 1 ]] || fail "the create under $fault never started copying, or finished first: the test proved nothing"
        case $fault in
            pause) redis-cli client pause 4000 ;;
            # a script that keeps the server busy, as a stalled engine would
            busy) redis-cli eval "local s = redis.call('TIME')[1]; while redis.call('TIME')[1] - s < 4 do end" 0 ;;
            partition)
                iptables -I INPUT -p tcp --dport 6379 -j DROP
                sleep 4
                iptables -D INPUT -p tcp --dport 6379 -j DROP ;;
        esac
        wait $job
        local rc ms
        read rc ms < $T/f$n.rc
        echo "create under $fault exited with $rc after $ms ms"
        [[ $ms -ge 3000 ]] || fail "the create outran the $fault fault, the test proved nothing"
        if [[ $rc -eq 0 ]]; then
            $WL manifest /jfs/.snapshots/f$n $T/f$n
            $WL compare $T/live $T/f$n --ignore atime || fail "snapshot taken under $fault is wrong"
        else
            cat $T/f$n.log
            listed f$n && fail "a create that failed under $fault left a snapshot"
        fi
    done
    SNAP_PATH=/tree snap after || fail "create after the faults failed"
}

# Creates and deletes racing each other, a name raced by several creates, and a
# limit raced by several creates, while another client keeps reading snapshots.
test_snapshot_lifecycle_races(){
    start_volume
    mount_client /jfs2
    $WL setup /jfs/bt 1
    local ok=0
    for i in $(seq 1 5); do
        ./juicefs snapshot create $META_URL --path /bt --name same > /dev/null 2>&1 &
    done
    for job in $(jobs -p); do
        wait $job && ok=$((ok + 1)) || true
    done
    [[ $ok -eq 1 ]] || fail "$ok creates of one name succeeded"
    ./juicefs config $META_URL --max-snapshots 3 --yes
    for i in $(seq 1 5); do
        ./juicefs snapshot create $META_URL --path /bt --name cap$i > /dev/null 2>&1 &
    done
    wait || true
    local count=$(snapshots | wc -l)
    [[ $count -le 3 ]] || fail "$count snapshots exist with a limit of 3"
    ./juicefs config $META_URL --max-snapshots 0 --yes
    for s in $(snapshots); do
        ./juicefs snapshot delete $META_URL --name $s
    done

    $WL read /jfs2/.snapshots 40 > $T/reader.log 2>&1 &
    local reader=$!
    local end=$((SECONDS + 35)) k=0
    while [[ $SECONDS -lt $end ]]; do
        k=$((k + 1))
        ./juicefs snapshot create $META_URL --path /bt --name c$k > /dev/null
        (( k > 2 )) && ./juicefs snapshot delete $META_URL --name c$((k - 2)) > /dev/null
    done
    wait $reader || { cat $T/reader.log; fail "a reader saw torn data while snapshots came and went"; }
    cat $T/reader.log
    stat /jfs2/.accesslog > /dev/null || fail "the reading client died"
    for s in $(snapshots); do
        ./juicefs snapshot delete $META_URL --name $s
    done
    rm -rf /jfs/bt
    ./juicefs gc $META_URL --delete
    [[ $(objects) -eq 0 ]] || fail "$(objects) objects leaked by create/delete races"
}

# dump and load keep snapshots whole, hard links and all, in both formats.
test_snapshot_dump_load(){
    for binary in "" "--binary"; do
        start_volume
        make_tree /jfs/tree
        SNAP_PATH=/tree snap s1
        $WL manifest /jfs/.snapshots/s1 $T/before
        ./juicefs dump $META_URL $T/dump $binary
        umount_jfs /jfs $META_URL
        python3 .github/scripts/flush_meta.py $META_URL
        ./juicefs load $META_URL $T/dump $binary
        mount_client /jfs
        $WL manifest /jfs/.snapshots/s1 $T/after
        $WL compare $T/before $T/after || fail "snapshot changed across dump and load $binary"
        ./juicefs fsck $META_URL --path /.snapshots/s1 --recursive || fail "fsck of a loaded snapshot failed"
    done
}

# fsck walks snapshots, by their path or as part of the whole volume, and finds
# an object one of them lost.
test_snapshot_fsck(){
    start_volume
    make_tree /jfs/tree
    SNAP_PATH=/tree snap s1
    rm -rf /jfs/tree
    ./juicefs gc $META_URL --delete
    ./juicefs fsck $META_URL --path /.snapshots/s1 --recursive || fail "fsck of a healthy snapshot failed"
    ./juicefs fsck $META_URL --path / --recursive || fail "fsck of a healthy volume failed"
    local victim=$(find /var/jfs/myjfs/chunks -type f | head -1)
    [[ -n "$victim" ]] || fail "the snapshot holds no object to lose"
    rm -f "$victim"
    if ./juicefs fsck $META_URL --path /.snapshots/s1 --recursive; then
        fail "fsck of the snapshot did not notice a lost object"
    fi
    if ./juicefs fsck $META_URL --path / --recursive; then
        fail "fsck of the volume did not notice an object lost by a snapshot"
    fi
}

# A client older than snapshots blocks the first one while it is mounted, and can
# no longer mount, nor run gc, once a snapshot exists.
test_snapshot_old_client(){
    if ! old_client_download /tmp/jfs-old; then
        [[ -n "$CI" ]] && fail "the 1.3 client could not be downloaded"
        echo "no 1.3 release reachable, skipped"
        return 0
    fi
    # older clients do not know the client-cache options of META_URL
    local old_url=${META_URL%%\?*}
    start_volume
    make_tree /jfs/tree
    /tmp/jfs-old/juicefs mount -d $old_url /jfs2 --cache-dir /var/jfsCache-jfs2
    wait_serving /jfs2
    if SNAP_PATH=/tree snap early; then
        fail "a snapshot was taken while a pre-snapshot client was mounted"
    fi
    umount_jfs /jfs2 $META_URL
    SNAP_PATH=/tree snap late || fail "snapshot after the old client left failed"
    if /tmp/jfs-old/juicefs mount -d $old_url /jfs2 --cache-dir /var/jfsCache-jfs2; then
        umount_jfs /jfs2 $META_URL
        fail "a pre-snapshot client mounted a volume with snapshots"
    fi
    if /tmp/jfs-old/juicefs gc $old_url --delete; then
        fail "a pre-snapshot client ran gc on a volume with snapshots"
    fi
}

# old_client_download DIR: the latest 1.3 release, the last line before snapshots,
# from the GitHub API, with GITHUB_TOKEN when set so that the rate limit of
# unauthenticated calls does not stop it
old_client_download(){
    local dir=$1 url
    local auth=()
    [[ -n "$GITHUB_TOKEN" ]] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
    url=$(curl -fsSL "${auth[@]}" "https://api.github.com/repos/juicedata/juicefs/releases?per_page=100" | python3 -c '
import json, sys
for r in json.load(sys.stdin):
    if r["tag_name"].startswith("v1.3.") and not r["prerelease"]:
        for a in r["assets"]:
            if a["name"].endswith("-linux-amd64.tar.gz"):
                print(a["browser_download_url"]); sys.exit()') || return 1
    [[ -n "$url" ]] || { echo "no 1.3 release listed"; return 1; }
    echo "downloading the old client from $url"
    mkdir -p $dir && curl -fsSL "$url" | tar -xz -C $dir juicefs || return 1
    $dir/juicefs version
}

source .github/scripts/common/run_test.sh && run_test $@
