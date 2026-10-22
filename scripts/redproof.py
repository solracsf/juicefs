"""For each standalone fix (commits 1..8 of final.json), revert its non-test
changes while keeping its test, run the tests it adds and expect them to fail;
then run them on the fix itself and expect them to pass.
usage: redproof.py 1 8   (env JFS_WT / JFS_GATE as in tplace.py)"""
import json, os, re, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tplace
from gate import dexec, tests_in_tree

HERE = os.path.dirname(os.path.abspath(__file__))
FINAL = json.load(open(os.path.join(HERE, 'final.json')))

for k in range(int(sys.argv[1]), int(sys.argv[2]) + 1):
    tplace.git('checkout', '-q', '-f', FINAL[k - 1])
    before = tests_in_tree()
    tplace.git('checkout', '-q', '-f', FINAL[k])
    tplace.git('clean', '-qfd')
    after = tests_in_tree()
    new = {p: sorted(after[p] - before.get(p, set())) for p in after}
    new = {p: v for p, v in new.items() if v}
    subj = tplace.git('log', '-1', '--format=%s').strip()
    code = [f for f in tplace.git('diff', '--name-only', FINAL[k - 1], FINAL[k]).split() if not f.endswith('_test.go')]
    res = {}
    for phase in ('without-fix', 'with-fix'):
        if phase == 'without-fix':
            tplace.git('checkout', '-q', FINAL[k - 1], '--', *code)
        else:
            tplace.git('checkout', '-q', '-f', FINAL[k])
        for p, names in new.items():
            rc, out, dt = dexec("go test ./%s -run '^(%s)$' -count=1 -timeout 5m -v 2>&1 | grep -E '^(\\s*--- |FAIL|ok|panic)|timed out|did not return|EPERM|EACCES|EBADF'" % (p, '|'.join(names)))
            fails = re.findall(r'--- FAIL: (\S+)', out)
            passes = re.findall(r'--- PASS: (\S+)', out)
            res[(phase, p)] = (fails, passes, out[-1500:])
    print('=== %d %s  code reverted: %s' % (k, subj, code))
    for (phase, p), (fails, passes, out) in res.items():
        print('  %-12s %-10s FAIL=%s PASS=%s' % (phase, p, fails, passes))
        if phase == 'without-fix' and not fails:
            print(out)
    tplace.git('checkout', '-q', '-f', FINAL[k])
