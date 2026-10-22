"""Initial placement of the target snapshot test declarations: for each one,
the latest feature group (index into the rebuilt history) that touched it in
the original history, by `git log -L`. Writes placement.json and newsubj.json.
Needs, in WORK/jfs: branch new2 (rebuilt code history) and tag pre-rebuild
(454516d9); WORK/tgt holds the target layout (mktarget.py)."""
import json, os, re, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from decls import decls

HERE = os.path.dirname(os.path.abspath(__file__))
JFS = os.path.join(HERE, 'jfs')
TGT = os.path.join(HERE, 'tgt')


def git(*a, cwd=JFS):
    return subprocess.run(['git', *a], capture_output=True, text=True, encoding='utf-8', cwd=cwd).stdout


new = git('rev-list', '--reverse', '9325fe3..new2').split()
newsubj = [git('log', '-1', '--format=%s', c).strip() for c in new]
group = {s: i for i, s in enumerate(newsubj)}
FIXES = newsubj[1:9]
HOLDS = group['feat(meta,cmd): hold snapshots against deletion']

old = git('rev-list', '--reverse', '9325fe3..pre-rebuild').split()
oldgroup = {}
for c in old:
    s = git('log', '-1', '--format=%s', c).strip()
    t = s[len('fixup! '):] if s.startswith('fixup! ') else s
    g = group.get(t, -1)
    if s in FIXES:
        g = -1
    b = git('log', '-1', '--format=%b', c)
    # the two fixups moved to the holds commit during the rebuild
    if "ListSnapshots reported a snapshot's Ctime" in b or "name LIKE 'juicefs.snapshot.hold.%'" in b:
        g = HOLDS
    oldgroup[c] = (g, s)

FILES = ['pkg/meta/snapshot_freeze_test.go', 'pkg/meta/snapshot_clone_test.go', 'pkg/meta/snapshot_consistency_test.go',
         'pkg/meta/snapshot_compat_test.go', 'pkg/meta/snapshot_surface_test.go', 'pkg/fs/snapshot_freeze_test.go',
         'cmd/snapshot_test.go']
res = {}
for f in FILES:
    _, _, ds = decls(open(os.path.join(TGT, f), encoding='utf-8').read())
    for name, text in ds:
        first = [l for l in text.split('\n') if not l.startswith('//')][0]
        if name.count('.') == 1 and first.startswith('func ('):
            t, mth = name.split('.')
            rx = '%s) %s(' % (t, mth)
        elif first.startswith('func '):
            rx = 'func %s(' % name
        elif first.startswith('type '):
            rx = 'type %s ' % name
        else:
            rx = first.split('=')[0].strip()
        log = git('log', '--format=%H', '-s', '-L', ':%s:%s' % (rx, f), 'pre-rebuild')
        cs = [c for c in log.split() if re.fullmatch(r'[0-9a-f]{40}', c)]
        gs = [oldgroup[c][0] for c in cs if c in oldgroup]
        res[f + '::' + name] = {'touch': sorted(set(gs)), 'group': max(gs + [-1]), 'n': len(cs)}
        print('%-60s %-3d %s' % ((f.split('/')[-1] + '::' + name)[:60], max(gs + [-1]), sorted(set(gs))))
json.dump(res, open(os.path.join(HERE, 'placement.json'), 'w'), indent=1)
json.dump(newsubj, open(os.path.join(HERE, 'newsubj.json'), 'w'), indent=1)
