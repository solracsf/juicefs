# The user's brief for the rebuild (2026-09-25, verbatim)

You are finishing a JuiceFS feature branch that adds read-only snapshots. Read AGENTS.md first and follow it (correctness first, engine parity across Redis, SQL and KV, minimal focused changes, tests for behavior changes, go fmt, US English comments).

## Setup
git fetch origin
git checkout feat/snapshot-reserved-range-integration
Then read HANDOFF.md in full. It is the source of truth for the state of the work, the remaining steps and the rules. Do not trust any other summary over the code.

## State
- Base: upstream juicedata/juicefs 9325fe3.
- feat/snapshot-reserved-range (9c897c59) holds the 19 feature commits. feat/snapshot-reserved-range-integration adds 40 audit-fix commits on top of them: 32 `fixup! <subject>` commits and 8 standalone upstream bug fixes, plus a final `chore: add handoff notes` commit.
- At this tip: build and vet are clean; the snapshot, clone and core meta suites pass; PostgreSQL passes with upstream PR #7576 applied; the battle suite (.github/scripts/command/snapshot.sh) passes 12/12 on Redis and SQLite. TiKV has not been run.

## Your job, in order (details in HANDOFF.md)
1. Point the new tests at Redis on 127.0.0.1:6379, using DB numbers no other test uses. Today they use private ports 6391 to 6394 in pkg/meta/snapshot_{freeze,clone,consistency,compat}_test.go. Fold each change into the commit that introduced the file.
2. Rebuild the history on 9325fe3:
   - commit 1 stays "fix(meta): return copies from memkv reads";
   - then the 8 standalone upstream fixes. Each must build and pass its own regression test at that position using only upstream APIs. Move their tests out of the snapshot_* test files into upstream-style test files for their area;
   - then the 18 feature commits with every fixup folded in (git rebase -i --autosquash, reordering the todo with GIT_SEQUENCE_EDITOR);
   - drop the handoff commit;
   - rewrite the message bodies of "fix(meta): keep older clients off and an existing /.snapshots reachable" (now a MetaVersion 1→2 bump on the first snapshot) and "feat(meta,cmd): take snapshots at one instant" (validation also checks hard-link identity and the directory-mtime window).
   Check that `git diff <tip before the rebuild> HEAD` shows only the test moves and the removed HANDOFF.md.
3. Per-commit gate, on the final history only. For every commit: go build ./... && go vet ./pkg/... ./cmd && go test ./pkg/meta -run 'TestSQLiteClient|TestMemKVClient' -count=1, plus that commit's own new tests. A MemKV "create snapshot: device or resource busy" failure appeared at one intermediate commit before the rebuild; it must not appear on any final commit. Root-cause it if it does.
4. Tip gate, on every engine you can run locally: Redis, SQLite, Badger, MemKV, MySQL, PostgreSQL (apply `git fetch https://github.com/juicedata/juicefs pull/7576/head` as a temporary diff, then revert it), etcd, FoundationDB (-tags fdb), and TiKV if you have it. Also pkg/vfs, pkg/fs, the cmd tests (TestArgsOrder|TestFormatMaxSnapshots|TestFormatVersion|TestSnapshotSubcommandFlags), and the battle suite with META=redis and META=sqlite3 (needs root, FUSE and the attr package). Root-cause every failure. Never skip, weaken or re-run a check to make it pass without proving the check itself wrong.
5. Force-push the result to origin feat/snapshot-reserved-range with --force-with-lease=feat/snapshot-reserved-range:9c897c5980b162f720e8f4492b6fdab1a4453730. Push the same commits to feat/snapshot-reserved-range-0df6sy.
6. Report: the final `git log --oneline 9325fe3..HEAD` with the commit count; the per-commit and tip gate results per engine; anything not run, with the reason; anything still open.

## Rules
- Author and committer on every commit: Git'Fellow <12234510+solracsf@users.noreply.github.com>. Set both user.name and user.email in this repo before rewriting.
- Every commit message ends with: Signed-off-by: Git'Fellow <12234510+solracsf@users.noreply.github.com>
- Conventional Commits. No Co-Authored-By, no AI attribution, no model names anywhere in the repository.
- Docs in English only (docs/en). Never touch docs/zh_cn.
- Nothing goes to juicedata/juicefs (no PR, no comment) without my explicit go-ahead.
- Confirm with me before any destructive action other than the force-push in step 5.

After the push, tell me what in the design summary is now out of date (https://claude.ai/artifact/V2qt87UrERP7sij4PvDNGy). It still describes the old MinClientVersion floor, and it overstates guarantees the audit showed were broken. Update it only if your session can publish artifacts.

Later instructions from the user (2026-09-25): push only if every gate is green, and show the final log and the diff against the pre-rebuild tip first; if the diff holds anything beyond test moves, Redis port changes and the removed HANDOFF.md, wait for the user's OK. Superseded on 2026-10-24: the base is upstream main at resume time (see NEXT-SESSION.md).
