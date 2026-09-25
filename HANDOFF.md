# Snapshot branch: handoff

This file only carries the work over to a local session. Drop it before the
history is rewritten; it must not end up in any commit that is kept.

## Where things stand

- Branch `feat/snapshot-reserved-range-integration` = the 19 feature commits of
  `feat/snapshot-reserved-range` (tip `9c897c59`, base upstream `9325fe3`) plus
  40 commits that fix every finding of the cold audit, then this note.
- `feat/snapshot-reserved-range` itself is untouched, still at `9c897c59`.
- Of the 40 fix commits, 32 are `fixup! <subject>` commits for one of the 19
  feature commits. The other 8 are standalone upstream bug fixes:
  - `fix(meta): refuse writes to immutable files`
  - `fix(fs): refuse writes through a handle opened without write access`
  - `fix(meta): keep the open checks on an open cache hit`
  - `fix(meta): watch the source chunks while cloning a file on Redis`
  - `fix(meta): count every occurrence of a slice when cloning a file on KV`
  - `fix(meta): copy FIFOs, sockets and devices when batch cloning on Redis`
  - `fix(meta): skip sources deleted during a batch clone on SQL and KV`
  - `fix(meta): make emptyDir terminate when an entry's inode is already gone`
  - plus commit 1, `fix(meta): return copies from memkv reads`, which was
    already standalone before the audit
- Every fix has a regression test that failed on `9c897c59` before the fix,
  except the non-zero exit of `snapshot delete` on failed object deletes
  (needs a broken object store and the sudo FUSE harness; reviewed only).

## Verified at this tip (same tree as the last gated commit)

- `go build ./...`, `go vet ./pkg/... ./cmd`: clean.
- `go test ./pkg/meta -run 'TestSnapshot|TestClone|TestSQLiteClient|TestMemKVClient|TestBadgerClient'`: ok.
- Full core meta suite (`SKIP_NON_CORE=true go test ./pkg/meta/...`, TiKV skipped): ok.
- PostgreSQL suite with upstream #7576 applied temporarily: ok.
- Battle suite `.github/scripts/command/snapshot.sh`, `META=redis` and `META=sqlite3`: 12/12 each.
- Not run: TiKV (unreachable from the cloud container); per-commit gate on the
  final restructured history (not built yet, see below).

## Remaining work, in order

1. **Test Redis ports.** The new test files point at private Redis servers:
   `pkg/meta/snapshot_freeze_test.go` (6391), `snapshot_clone_test.go` (6392),
   `snapshot_consistency_test.go` (6393), `snapshot_compat_test.go` (6394).
   Upstream CI only has Redis on 6379, so point them at `127.0.0.1:6379` with DB
   numbers no other test uses (grep existing tests for `redis://127.0.0.1` and
   `/N`). Fold each change into the commit that introduced the file.
2. **Restructure the history** on `9325fe3`:
   - commit 1 stays `fix(meta): return copies from memkv reads`;
   - then the standalone upstream fixes above, each building and passing its own
     regression test at that position with upstream APIs only. Their tests
     currently sit in the `snapshot_*_test.go` files next to snapshot tests;
     move each into an upstream-style test file of its area so the commit
     compiles without snapshot code;
   - then the 18 feature commits with every `fixup!` folded in (`git rebase -i
     --autosquash 9325fe3`, reordering the todo with `GIT_SEQUENCE_EDITOR`);
   - rewrite the message body of commits whose behavior changed materially:
     at least `fix(meta): keep older clients off and an existing /.snapshots
     reachable` (now a metadata-version bump, `MetaVersion` 1 to 2 on the first
     snapshot, not a `MinClientVersion` floor) and `feat(meta,cmd): take
     snapshots at one instant` (validation also checks hard-link identity and
     distrusts directories listed within the directory-mtime skip window);
   - check `git diff <pre-restructure tip> HEAD` shows only the test moves.
3. **Per-commit gate on the final history only**: every commit builds, vets and
   passes `go test ./pkg/meta -run 'TestSQLiteClient|TestMemKVClient'` plus its
   own new tests. A MemKV `create snapshot: device or resource busy` failure was
   seen at an intermediate fixup commit in the unsquashed history and fixed by
   the next one; it must not show on any final commit.
4. **Tip gate**: every engine available (Redis, SQLite, Badger, MemKV, MySQL,
   PostgreSQL with #7576, etcd, FoundationDB with `-tags fdb`), `pkg/vfs`,
   `pkg/fs`, the `cmd` tests (`TestArgsOrder|TestFormatMaxSnapshots|
   TestFormatVersion|TestSnapshotSubcommandFlags`), and the battle suite on
   Redis and SQLite. Root-cause any failure; never skip or weaken a check.
5. Force-push the result to `feat/snapshot-reserved-range` (lease on
   `9c897c59`), then update the design summary artifact
   (https://claude.ai/artifact/V2qt87UrERP7sij4PvDNGy): it still describes the
   old `MinClientVersion` floor and overstates the guarantees the audit broke.

## Rules

- Author and committer: `Git'Fellow <12234510+solracsf@users.noreply.github.com>`.
- Every commit ends with `Signed-off-by: Git'Fellow <12234510+solracsf@users.noreply.github.com>`.
- Conventional Commits; no Co-Authored-By, no AI attribution anywhere.
- Docs in English only (`docs/en`); never touch `docs/zh_cn`.
- Nothing goes to `juicedata/juicefs` without an explicit go-ahead.

## Open points to mention upstream

- Tools that never register a session (for example `gc` or `restore`) and
  started before the first snapshot are not seen by the check that refuses a
  snapshot while a client without snapshot support is running; they are refused
  the next time they load the format.
- `nextSnapshot=0` is written on every load, like the existing `nextTrash`
  counter; it has no visible effect.
- `juicefs config` with no flags on an old client still prints the format of a
  snapshotted volume without a version check; it writes nothing.
