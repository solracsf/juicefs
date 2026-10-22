# JuiceFS snapshots: rebuild kit

Everything needed to rebuild the snapshot branch, a second time, without
rediscovering anything. The first rebuild (session of 2026-09-25) produced a
green 27-commit history (tip 52f6bade) that lived only in a session scratchpad
and was lost when the scratchpad was wiped. This kit is that work, saved.

**Rule for the rebuild session: persist as you go.** Keep the work clone and
worktrees under `D:\GitHub\` (never a scratchpad), and after each phase push
the result to the fork under a backup name, e.g.
`git push origin <sha>:refs/heads/snapshot/rebuild-wip` (solracsf/* writes are
allowed). Gate logs go in this kit (`gate-logs/`), committed.

## Goal and acceptance (the user's brief, condensed)

1. New tests use Redis on 127.0.0.1:6379 with DB numbers no other test uses
   (done in the target layout: freeze/immutable 4, clone 5, consistency 6,
   compat 9).
2. History on the base: commit 1 `fix(meta): return copies from memkv reads`;
   then the 8 standalone upstream fixes, each building and passing its own
   regression test at its position with upstream APIs only, tests in
   upstream-style files; then the 18 feature commits with every `fixup!` folded
   in; handoff commit dropped. Rewrite the bodies of commit 15 (MetaVersion
   1->2) and commit 19 (directory-mtime window). `git diff <old tip> HEAD`
   shows only test moves, port changes and the removed HANDOFF.md (it also
   shows the two new upstream-API tests and the newTestMetas cleanup: the
   user is told and must OK before the push).
3. Per-commit gate on the final history: build, vet, SQLite + MemKV suites,
   the commit's own new tests; no MemKV "device or resource busy" anywhere.
4. Tip gate on Redis, SQLite, Badger, MemKV, MySQL, PostgreSQL, etcd,
   FoundationDB, TiKV; pkg/vfs, pkg/fs, cmd tests
   (TestArgsOrder|TestFormatMaxSnapshots|TestFormatVersion|TestSnapshotSubcommandFlags);
   battle suite META=redis and META=sqlite3. Root-cause every failure.
5. Force-push solracsf/juicefs `feat/snapshot-reserved-range` with
   `--force-with-lease=feat/snapshot-reserved-range:9c897c5980b162f720e8f4492b6fdab1a4453730`,
   and the same commits to `feat/snapshot-reserved-range-0df6sy`. Only after
   the user's OK on the diff.
6. Author and committer Git'Fellow <12234510+solracsf@users.noreply.github.com>,
   Conventional Commits, `Signed-off-by: Git'Fellow <...>` last, no AI
   attribution anywhere. Docs in docs/en only, never docs/zh_cn. Nothing to
   juicedata/juicefs without the user's explicit go-ahead.

## Refs

Local branches in `D:\GitHub\juicefs` (pinned 2026-10-22, never check them out
in that shared checkout; use worktrees or a clone):

| Branch | SHA | What |
|---|---|---|
| `snapshot/integration-454516d9` | 454516d9 | the input: 19 feature commits + 40 audit commits + handoff (= origin/feat/snapshot-reserved-range-integration) |
| `snapshot/pre-audit-9c897c59` | 9c897c59 | what the fork's two target branches hold today |
| `snapshot/upstream-main` | 5828217a | juicedata main on 2026-10-22 |
| `snapshot/pr7576` | 0147984d | PR #7576 head (two commits; merged upstream as a5ee4993) |

Base used last time: upstream `9325fe3e`.

## Open decision before rebuilding: which base

Upstream main is 62 commits past 9325fe3 (14 files in pkg/meta). #7576 is
merged (a5ee4993), so on current main the PostgreSQL leg needs no temporary
patch. But `meta/sql: use multi-row INSERT in batch clone (#7598)` rewrites the
SQL batch clone that standalone fix 7 changes, and pkg/meta/utils.go changed
near fix 8; expect new conflicts there. None of the 8 fixes is upstream yet.
Option A: rebuild on 9325fe3 exactly as the ledger below (fastest, known).
Option B: rebuild on upstream main (needs resolving #7598 against fix 7 and
re-checking each fix's red/green on the new base). Ask the user.

## Environment (as of 2026-10-22 global rules)

- Runtimes (python, go) run in WSL or Docker, not on Windows: read
  `~/.claude/docs/wsl-test.md` first. Last time python and gofmt ran on the
  Windows host; the scripts only need python3, git and gofmt on PATH (gofmt
  from golang:1.25 or a WSL Go install).
- Build/test always in `golang:1.25` containers with volumes
  `juicefs-gomod:/go/pkg/mod` and `juicefs-gobuild:/root/.cache/go-build`,
  `-e GOFLAGS=-buildvcs=false`. From Git Bash prefix `MSYS_NO_PATHCONV=1` on
  every `docker run`/`docker exec` with container paths.
- The work clone must be LF: `git clone -c core.autocrlf=false --no-checkout D:/GitHub/juicefs <WORK>/jfs`,
  then set `origin` to https://github.com/solracsf/juicefs.git, fetch, set
  user.name/user.email to Git'Fellow. Never operate on D:\GitHub\juicefs's
  checkout (shared with other sessions).
- WORK layout the scripts expect: `<WORK>/` holds the scripts (copy `scripts/*`
  there), `<WORK>/jfs` (clone), `<WORK>/tgt` (target worktree),
  `<WORK>/wt`, `<WORK>/wt2`, ... (gate worktrees), `<WORK>/carry/`,
  `<WORK>/msgs/` (copy `msgs/*`). Put WORK under D:\GitHub, e.g.
  `D:\GitHub\juicefs-rebuild-work`.
- Other sessions run containers here: name yours with a prefix (`snapgate`,
  `snaptip`, `snaptikv`, `snapbt-*`) and remove them when done.

## Procedure

1. **Clone and tags.** In `<WORK>/jfs`: `git tag pre-rebuild 454516d9`.
2. **Target layout.** `git worktree add -b target <WORK>/tgt pre-rebuild`,
   `git rm HANDOFF.md`, `python mktarget.py <WORK>/tgt`, `gofmt -w` the touched
   test files, commit on branch `target`. Its tree is the required final tree.
   Check `git diff --stat pre-rebuild target`: 11 files (HANDOFF.md,
   pkg/fs/fs_test.go, pkg/fs/snapshot_freeze_test.go, pkg/meta/clone_test.go,
   immutable_test.go, openfile_test.go, snapshot_clone_test.go,
   snapshot_compat_test.go, snapshot_consistency_test.go,
   snapshot_freeze_test.go, utils_test.go), about +721 -658.
3. **Strip the snapshot test files** from a copy of the history, so the rebase
   only meets code: `git branch -f h0 pre-rebuild` then
   `FILTER_BRANCH_SQUELCH_WARNING=1 git filter-branch -f --index-filter "git rm -q --cached --ignore-unmatch 'pkg/meta/snapshot_*_test.go' 'pkg/fs/snapshot_freeze_test.go'" 9325fe3..h0`.
4. **Reorder rebase.** `git switch -c new2 h0`, then `../drive.sh start`.
   It stops at each conflict the whitespace resolver cannot take
   (STOP-MANUAL) and whenever a carried hunk becomes applicable (STOP-OTHER).
   Resolve each stop per the ledger below, check with `../msdiff.sh <file>`,
   `git add`, then `../drive.sh continue`. Tools: `hk.py list|apply|carry`,
   `carry_apply.py`, `movefix.py`, `r19.py`, `r20.py`, `r23.py`.
   Persist: push `new2` to the fork as `snapshot/rebuild-wip-code` right after.
5. **Verify the code.** `git diff --stat h0 new2` must list only HANDOFF.md
   and cmd/snapshot_test.go (both removed on purpose).
6. **Build every commit** (`buildall.sh` in golang:1.25 with the clone at
   /src). Expected, and handled by `tplace.code_patches`: commits 10..22 miss
   the `unicode` import; commit 19 has `undefined: c` (trusts check). Anything
   else is a real failure.
7. **Place the tests.** `git rev-list --reverse 9325fe3..new2 > newshas` as JSON
   (`newshas.json`), `python place.py`, `python init_state.py`, start the gate
   container (`snapgate`, network `container:snapgate-redis`, worktree `wt` of
   the clone at /w), then `python tplace.py compile 0 26`.
8. **Assemble.** `python assemble.py` (env JFS_WT to a free worktree). Check
   `git rev-parse final^{tree}` equals `git rev-parse target^{tree}`. Push
   `final` to the fork as `snapshot/rebuild-wip`.
9. **Gates** (below). Any test change means: edit target (or state.json),
   re-run assemble, verify the tip tree again, re-gate the commits whose trees
   changed (compare trees with the previous final.json).
10. **Report to the user**: `git log --oneline 9325fe3..final`, the diff
    `git diff pre-rebuild final` (send the file), gate results, what was not
    run. Push only after the user's OK (step 5 of the goal).
11. After the push: update the design summary artifact
    https://claude.ai/artifact/V2qt87UrERP7sij4PvDNGy (see "Artifact" below).

## Conflict ledger (rebase of h0, base 9325fe3)

SHAs in parentheses are h0 / integration-branch ones; match by subject.
"Carry" = `hk.py carry`, applied later by `carry_apply.py` when the carry
check stops the rebase.

1. fix `watch the source chunks while cloning a file on Redis` (998ec6f7 /
   1e06ef10), redis.go: resolve.sh handles it (whitespace-only: the fix wraps
   doCloneEntry/doBatchClone in a retry loop). `errors` is github.com/pkg/errors,
   already imported.
2. pick `reserve an inode range...`, redis.go: resolve.sh.
3. fixup trash counter (468a3c35 / c79cbd18), base.go: apply hunk 2 (the
   trashCounterInRange/resetTrashCounter helpers); carry hunks 0 and 1
   (buildSnapshot skips taken snapshot inodes; `window, err :=`). Engines merge
   cleanly.
4. fixup both spellings (c25b283b / faeff4b5), base.go: at commit 9 turn every
   `isReservedEntry(` (7 call sites: Mknod, Link, Unlink, Rmdir, Rename x2,
   Clone) into `m.isReservedEntry(` and define, right after ensureSnapshotRoot:
   `func (m *baseMeta) isReservedEntry(parent Ino, name string) bool { return parent == RootInode && (isTrashName(name) || isSnapshotName(name)) }`
   with the comment ending "...normal namespace calls." (no hidesEntry clause
   yet). interface.go merges.
5. fixup volume root only (1b5fdc9c / 76d9a0a6): add atVolumeRoot before
   isReservedEntry, body `parent == RootInode && isTrashName(name) || m.atVolumeRoot(parent) && isSnapshotName(name)`,
   comment adds "The trash is reserved under the root of every mount, as it
   always was."; carry the hidesEntry part (apply at commit 15).
6. pick `create and list snapshots` (e898e0d8): `hk.py apply pkg/meta/base.go '0,1@m.atVolumeRoot(parent) && isSnapshotName(name)+2,2'`
   and `hk.py apply pkg/meta/interface.go '0@var SnapshotName = ".snapshots"+1,1'`.
7. fixup clonePublicModes (d6028ffc / cd1dacf0): by hand, put the
   clonePublicModes const before Clone and the `cmode&^clonePublicModes != 0`
   EINVAL check first in Clone (context differs only by `m.isReservedEntry`).
8. fixup created-time xattr (363393dd / 627ddf66): built on holds. Move it:
   `python ../movefix.py 363393dd <prefix of the holds pick SHA> "created-time xattr"`,
   `git reset -q --hard HEAD`, `GIT_EDITOR=true git rebase --skip`.
9. fixup subcommand flags (305c2066 / bafe72e0), cmd/snapshot.go: at commit 10
   remove the parent Flags; create gets `Flags: []cli.Flag{ &cli.StringFlag{Name: "path", Value: "/", Usage: "the directory to snapshot, relative to the volume root"}, snapshotNameFlag() }`;
   add `func snapshotNameFlag() cli.Flag` (name, "name of the snapshot") before
   snapshotCreate. Keep cmd/main.go and cmd/main_test.go (reorderOptions).
   Remove cmd/snapshot_test.go (placed at 23 by the test placement).
10. fixup dump refuses snapshot subdir (4deb6a95 / 78ce69ed), commit 11: insert
    checkDumpRoot before the `// CreateSnapshot freezes a copy` comment
    (`hk.py apply pkg/meta/base.go '0@// CreateSnapshot freezes a copy+-1'`) and
    the call as the first statement of DumpMeta in redis/sql/tkv
    (`hk.py apply pkg/meta/<e>.go "0@func (m *<T>) DumpMeta(w io.Writer"`).
11. fixup comment move (90d26480 / 0aa339e8): at commit 11 the comment is
    already right; `git checkout --ours pkg/meta/base_test.go`. Fixed at 19.
12. pick `keep older clients off...` (f17526ef), commit 15:
    `hk.py apply pkg/meta/base.go 0,4,7,8,9,10`, engines hunk 1; Unlink and
    Rmdir use `m.hidesEntry(ctx, parent, name)`, Rename uses
    `m.hidesEntry(ctx, parentSrc, nameSrc) || m.isReservedEntry(parentDst, nameDst)`;
    the isReservedEntry comment becomes "...calls; hidesEntry says whether the
    name resolves to one here. The trash is reserved under the root of every
    mount, as it always was."; hidesEntry goes right after isReservedEntry;
    storedFormat, belowVersion, raiseMinClientVersion before checkDumpRoot,
    dumpedFormat and checkDumpedFloor after it; in each engine the
    `setting, err := m.dumpedFormat()` block plus the deferred
    checkDumpedFloor go right after the checkDumpRoot call. msdiff shows only
    those expected differences. Then the carry check stops: apply carry 002
    (hidesEntry honors atVolumeRoot) and amend.
13. fixup MetaVersion (ca97927d / f5a78b17): `HK_U=1 hk.py apply pkg/meta/base.go 0,...,11`
    (all 12 U1 hunks match); interface.go: `HK_U=1 hk.py apply ... 0`, delete
    the MinSnapshotVersion const, add to SessionInfo after `Version string`:
    `// MetaVersion is the highest metadata version the client supports; clients`
    / `// that predate snapshots leave it out` / `MetaVersion int \`json:",omitempty"\``.
14. fixup SQL hold prefix case (10966b02 / 8819df49): built on holds; move it
    like step 8 (`movefix.py 10966b02 <holds pick prefix> "hold prefix case on SQL"`).
15. fixup delete deadlock (cb693af5 / dc861a25): U1 hunks 0,1; by hand add the
    comment lines and `m.deleteWaitMu.Lock()`/`Unlock()` around the two drain
    loops, which are still `for i := 0; i < cap(m.maxDeleting); i++ {` here.
16. fixup failed object deletes (8078480f / e42adc86), cmd/snapshot.go: the
    DeleteSlice callback is still `func(args ...interface{}) error`; add
    failedSlices counting and the error after DeleteSnapshot; also add
    `Flags: []cli.Flag{snapshotNameFlag()},` to the delete subcommand.
17. pick `take snapshots at one instant` (05d69ad6), cmd/snapshot.go: U1 hunks
    0,2; the --best-effort BoolFlag goes into create's Flags with usage "keep
    the copy even if the tree changes while it is taken". base_test.go:
    move the "Compacting a file merges..." comment from above
    testSnapshotConsistency down onto testSnapshotCompact.
18. fixup mtime window (e91ce77e / db1ca3aa): `python ../r19.py`, then
    `HK_U=1 hk.py apply pkg/meta/sql.go 0`, `... tkv.go 0,1,2`, gofmt, msdiff
    (only hard-link parts and alignment differ). Then carry 000 fires: apply it,
    and by hand change `if build.window, err = m.dirMtimeWindow(); err != nil {...}`
    to `if window, err := m.dirMtimeWindow(); err != nil { return 0, errno(err) } else { build.window = window }`;
    mark carry 001 done; amend.
19. pick `preserve hard links in snapshots` (b57e9234): `python ../r20.py`, then
    `HK_U=1 hk.py apply pkg/meta/base_test.go '0,1@// Compacting a file merges its slices+-1'`.
20. fixup hard-link identity (f19ba5e4 / fb11e566): `HK_U=1 hk.py apply pkg/meta/base.go 1,2,...,11`,
    then replace the 3 snapshotMatches comment lines with the 4-line version
    ending "...and the same files behind the names of hard links. Access times
    and link / counts are left out: reads move the former, and the latter only
    count the / links a snapshot copies."
21. pick `hold snapshots against deletion` (0e129c19): `python ../r23.py`. msdiff
    differs only in the flag layout. The two moved fixups then apply cleanly.
22. pick refactor (77ffa869): base.go U1 hunks 0,2,3 plus both drain loops to
    `for range cap(m.maxDeleting) {`; cmd/snapshot.go: `...interface{}` to
    `...any`, both progress goroutines get a `time.NewTicker(200 * time.Millisecond)`
    with `case <-ticker.C:`, create's goroutine loads total and count with
    atomic.LoadUint64. msdiff empty.

Result last time: `git diff h0 new2` = HANDOFF.md and cmd/snapshot_test.go only.

## Test placement facts

- Commit indexes: 0 memkv, 1-8 fixes, 9 reserve, 10 create/list, 11 gc/dump,
  12 exempt, 13 refuse attrs, 14 vfs, 15 older clients, 16 delete, 17 cap,
  18 atime, 19 one instant, 20 hard links, 21 fsck, 22 xattr ctime, 23 holds,
  24 refactor, 25 battle suite, 26 docs.
- `TestSnapshotRaisesMetaVersion` and `TestSnapshotRefusesUnawareSessions`
  compile only from 16 (the compile loop finds it).
- `TestSnapshotSkipsTakenInodes` must be at 19 (init_state.py sets it).
- `TestFastResolveHiddenRoots` (pkg/fs/fs_test.go, came with commit 14) calls
  the 6-argument CreateSnapshot; code_patches strips the argument before 19.

## Commit messages

`msgs/NN.txt` (NN = commit index) replace the new2 messages for 8 to 25;
others keep their message. 15 and 19 are the two the user required. The
hard-link identity check is described in 20, not 19: hard links are only
preserved from 20, so at 19 that check would reject every tree with links.

## Gates

- Per-commit: `python gate.py 0 26` (container `snapgate` with its own Redis,
  worktree `wt`). Last time: 27/27, no EBUSY. Run a second container
  (`snapgate2`, `wt2`) for parallel ranges.
- Red proof: `JFS_WT=<WORK>/wt2 JFS_GATE=snapgate2 python redproof.py 1 8`.
  Last time all 8 failed without the fix and passed with it.
- Tip: containers sharing one network namespace: `snaptip-redis` (redis:7-alpine),
  `snaptip-mysql` (mysql:8.0, MYSQL_ALLOW_EMPTY_PASSWORD=yes MYSQL_DATABASE=dev),
  `snaptip-pg` (postgres:16, POSTGRES_USER=root POSTGRES_DB=test
  POSTGRES_HOST_AUTH_METHOD=trust), `snaptip-etcd` (quay.io/coreos/etcd:v3.5.17
  `etcd --listen-client-urls http://127.0.0.1:3379 --advertise-client-urls http://127.0.0.1:3379 --listen-peer-urls http://127.0.0.1:3380`),
  runner `snaptip` (golang:1.25, -e ETCD_ADDR=127.0.0.1:3379, worktree at /w):
  `docker cp tip/tipgate.sh snaptip:/tipgate.sh; MSYS_NO_PATHCONV=1 docker exec snaptip bash /tipgate.sh`.
  PostgreSQL: on base 9325fe3, a worktree with `git cherry-pick -n snapshot/pr7576`
  (discard after); `go test ./pkg/meta -run TestPostgreSQL`.
  TiKV: separate namespace `snaptikv-redis`; `pingcap/pd:v7.5.1 --name=pd --client-urls=http://127.0.0.1:2379 --peer-urls=http://127.0.0.1:2380 --advertise-client-urls=http://127.0.0.1:2379 --advertise-peer-urls=http://127.0.0.1:2380 --data-dir=/data/pd`,
  then `pingcap/tikv:v7.5.1 --pd=127.0.0.1:2379 --addr=127.0.0.1:20160 --data-dir=/data/tikv`;
  run `go test ./pkg/meta -run 'TiKV|Tikv|^TestLoadDump$'` (full TestLoadDump
  needs TiKV and Redis). FoundationDB: `tip/fdb.sh` in a throwaway golang
  container.
- Battle suite: build `make juicefs` from final in golang:1.25; copy the
  binary and `.github` into a dir; image `jfs-bt` (ubuntu:24.04 with redis,
  fuse3, pip minio; still present on this host), `docker run --rm --privileged`:
  `cp -r /src/. /work/ && cd /work && chmod +x juicefs .github/scripts/command/*.sh`,
  for Redis first `redis-server --daemonize yes` (no service manager in the
  container: without it the run dies at flush_meta), then
  `META=redis .github/scripts/command/snapshot.sh` (and META=sqlite3). About
  9 minutes each, 12 tests.

## Known outcomes (do not re-chase)

- `TestFastResolveHiddenRoots` fails intermittently with "TempDir RemoveAll
  cleanup: ... directory not empty": the access log written after close races
  t.TempDir cleanup. Upstream 9325fe3 shows the same with TestWebdav (1/20).
  Report it, do not re-run to green.
- `TestLoadDumpSlow` needs a Redis Cluster on :7001; `TestKeyDB` needs :6378.
  Not run.
- `TestRedisCache` runs FlushAll on 6379: any test leaving a session open on
  6379 then exits the binary through refresh ("database is not formatted").
  newTestMetas closes its sessions; keep it that way.
- One unexplained `TestSnapshotReadOnlyHandle` failure at commit 10 in a
  gate run (0.00s), 10/10 green when re-run alone; the gate now keeps full
  output so a recurrence can be diagnosed.
- The `snapshot delete` non-zero exit on failed object deletes has no test
  (needs a broken object store); reviewed only.

## Artifact (design summary) after the push

https://claude.ai/artifact/V2qt87UrERP7sij4PvDNGy (owned, private). Out of
date: tip and commit count (27); the MinClientVersion floor (now MetaVersion 2:
storage model, create diagram, compatibility, crash table, review list);
guarantees the audit broke (Frozen: immutable writes, read-only handles,
open-cache hits, public CLONE_MODE_SNAPSHOT, SetAttr raising FlagSnapshot,
chmod/chattr on /.snapshots; Point in time: SkipDirMtime window, hard links
split during the copy, deletes during batch clone; Data stays: Redis
compaction race, KV duplicate slices); delete semantics (wait only without a
session, serialized concurrent deletes, non-zero exit on failed deletes); holds
(SQL exact-case prefix, creation time); interactions (sweep spares builds, dump
of a snapshot subdir refused, fsck repair keeps the freeze, both spellings
reserved, fast-resolve fallback, trash counter reset); known limits (drop
"format rewrites race", add tools without a session); verification table and
commit map. Suggested: a new "What the audit changed" section after the
contract. Read the artifact in full before republishing.

## Upstream issue draft

`docs/issue-draft.md`, juicedata enhancement template. On hold; post only on
the user's explicit "post it".

## Suggested split for the rebuild session

- Orchestrator: decides base (ask user), runs steps 1-3, owns pushes.
- One worker (Sonnet high, or Opus medium if the base is upstream main): the
  rebase, steps 4-6, following the ledger; it pushes new2 backup.
- In parallel once new2 exists: test placement and assembly (Sonnet medium);
  then gates fan out: per-commit ranges on 2 containers, tip engines, battle
  suite, red proof (Haiku or Sonnet low each, they only run and report).
- Orchestrator: verify tip tree, collect results, report, push on OK, artifact.
