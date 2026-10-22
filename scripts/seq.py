"""GIT_SEQUENCE_EDITOR for the rebuild: commit 1, then the 8 standalone fixes,
then the features with their autosquashed fixups; drops the handoff commit and
adds a carry check after every line. Matches by subject, so it works on any
rewrite of the integration history (h0 has new SHAs)."""
import sys
fixes = ["fix(meta): refuse writes to immutable files",
 "fix(fs): refuse writes through a handle opened without write access",
 "fix(meta): keep the open checks on an open cache hit",
 "fix(meta): watch the source chunks while cloning a file on Redis",
 "fix(meta): count every occurrence of a slice when cloning a file on KV",
 "fix(meta): copy FIFOs, sockets and devices when batch cloning on Redis",
 "fix(meta): skip sources deleted during a batch clone on SQL and KV",
 "fix(meta): make emptyDir terminate when an entry's inode is already gone"]
p = sys.argv[1]
todo = [l for l in open(p).read().splitlines() if l and not l.startswith('#')]
fl, rest = {}, []
for l in todo:
    subj = l.split(' ', 2)[2].lstrip('# ').strip()
    if subj in fixes: fl[subj] = l
    elif 'handoff' in subj: continue
    else: rest.append(l)
assert len(fl) == 8, fl
out = rest[:1] + [fl[f] for f in fixes] + rest[1:]
out = [x for l in out for x in (l, 'exec python ../carry.py check')]
open(p, 'w').write('\n'.join(out) + '\n')
open(p + '.copy', 'w').write('\n'.join(out) + '\n')
