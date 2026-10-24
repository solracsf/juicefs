# Start here: resuming the JuiceFS snapshot rebuild

Paste this as the first message of a new session opened in `D:\GitHub\juicefs`:

```
Resume the JuiceFS snapshot rebuild from the kit on branch snapshot-rebuild-kit
(worktree D:\GitHub\juicefs-rebuild-kit). Read NEXT-SESSION.md, README.md and
docs/original-brief.md there first, then ~/.claude/CLAUDE.md and the docs it
lists for git, Docker and WSL. Do not start until I say the rebuild is off hold.
```

## State on 2026-10-24

- Fork solracsf/juicefs: `feat/snapshot-reserved-range` and
  `feat/snapshot-reserved-range-0df6sy` still hold the pre-audit tip 9c897c59.
  `feat/snapshot-reserved-range-integration` (454516d9) holds the input: 19
  feature commits, 40 audit commits, a handoff commit.
- A green 27-commit rebuild was made on 2026-09-25 (tip 52f6bade, never
  pushed) and lost with the session scratchpad. This kit is everything needed to
  redo it: README.md (procedure, conflict ledger, placement facts, gate recipes,
  known outcomes, artifact update list), scripts/, msgs/, tip/.
- On hold by the user: the rebuild itself, and posting the upstream issue
  (docs/issue-draft.md). Wait for the user's go-ahead on both.
- Local pins in D:\GitHub\juicefs (never check them out in that shared
  checkout): snapshot/integration-454516d9, snapshot/pre-audit-9c897c59,
  snapshot/upstream-main (5828217a on 2026-10-22), snapshot/pr7576.

## Decided

- **Base: upstream main as fetched at resume time** (routing decision,
  2026-10-24), not 9325fe3. Reason: a rebuild on the September base would need a
  second rebase before any PR; the ledger tracks the commits by intent, not by
  line. Fallback to 9325fe3 only if replaying onto main proves impossible, and
  report that to the routing session.
- On main, #7576 is already merged (a5ee4993): the PostgreSQL leg needs no
  temporary patch. Expect new conflicts: #7598 (multi-row INSERT in SQL batch
  clone) against standalone fix 7, and pkg/meta/utils.go changes near fix 8.
  None of the 8 standalone fixes is upstream yet (checked 2026-10-22); check
  again.

## First steps when resumed

1. `git -C D:\GitHub\juicefs fetch origin` and fetch upstream main
   (`git fetch https://github.com/juicedata/juicefs main:refs/heads/snapshot/upstream-main`).
2. Check the ledger against the main of the day: for each of the 8 standalone
   fixes and each ledger step, confirm the code it touches still exists on main
   and whether upstream fixed it meanwhile. Report changes to routing.
3. Send a `ROUTING PLAN juicedata/juicefs juicefs-snapshot-rebuild` to the routing
   session before any subagent (task list, models, tests), and wait for its reply.
4. Follow README.md "Procedure", with `9325fe3` replaced by the chosen base.

## Rules that changed since September (read ~/.claude/CLAUDE.md for the text)

- Runtimes (python, go, pytest) run in WSL or Docker, not on Windows python.
- An orchestrating session never writes code, tests or docs itself: workers do,
  after a ROUTING PLAN approved by the routing session. Every decision a session
  would put to the user goes to the routing session, except GitHub writes outside
  solracsf/cloudeezy/site24h, production writes, deletions, purchases and
  settings changes.
- Any juicedata/* GitHub write (issue, PR, comment) is the user's decision, on an
  explicit "post it" / "open it".
- Persist as you go: never keep the only copy of work in a scratchpad. Work clone
  and worktrees under D:\GitHub; push every phase to the fork under a backup name
  (e.g. `snapshot/rebuild-wip`). The `scratch_rescue` hook bundles unpushed
  scratchpad repos to D:\GitHub\.scratch-rescue at each turn end, as a safety
  net only.
- Language: everything in English (code, commits, docs, GitHub, messages between
  sessions); replies to the user in French, one short line when something is
  needed, nothing otherwise.
- Tests and debugging kill only processes they started (never by image name).

## When the rebuild is done

Show the user `git log --oneline <base>..final` and the diff against the
pre-rebuild tip, then force-push only on the user's OK (lease and branches in
README.md, step 5 of the goal). Then update the design summary artifact
(README.md, "Artifact").
