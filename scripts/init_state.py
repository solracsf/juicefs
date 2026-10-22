"""Initial test placement (state.json) from placement.json (place.py).
Fix tests sit on their fix commit; snapshot tests no earlier than commit 9;
cmd/snapshot_test.go needs delete, hold and --best-effort (commit 23);
TestSnapshotSkipsTakenInodes tests code that lands in commit 19.
tplace.py compile then bumps whatever does not compile."""
import json, os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tplace
T = tplace.load_target()
pl = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), 'placement.json')))
st = {}
fixmap = {'pkg/meta/immutable_test.go': 1, 'pkg/meta/openfile_test.go': 3, 'pkg/meta/utils_test.go': 8, 'pkg/fs/fs_test.go': 2}
clone = {'TestCloneDupSliceRefs': 5, 'TestCloneSpecialFiles': 6, 'unlinkDuringBatch': 7, 'makeTreeWithVictims': 7,
         'checkCopiedDir': 7, 'TestCloneDeleteDuringCopy': 7}
for f, (h, imps, ds) in T.items():
    for n, t in ds:
        key = f + '::' + n
        if f in fixmap: st[key] = fixmap[f]
        elif f == 'pkg/meta/clone_test.go': st[key] = clone.get(n.split('.')[0], 4)
        elif f == 'cmd/snapshot_test.go': st[key] = 23
        else: st[key] = max(9, pl[key]['group'])
st['pkg/meta/snapshot_compat_test.go::TestSnapshotSkipsTakenInodes'] = 19
json.dump(st, open(tplace.STATE, 'w'), indent=1)
print(len(st))
