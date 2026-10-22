"""Per-commit gate on the assembled history (final.json, written by assemble.py).

usage: gate.py FROM TO   (commit indexes into final.json)
For each commit: check it out in the worktree, then in the gate container run
build, vet, the SQLite and MemKV meta suites and the Test functions the commit
introduces (compared with the previous commit, or upstream 9325fe3 for commit
0). Logs go to gate/<k>.log; one summary line per commit.
Env: JFS_WT, JFS_GATE as in tplace.py.
"""
import json, os, re, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tplace

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, 'gate')
os.makedirs(OUT, exist_ok=True)


def dexec(cmd, timeout=3600):
    t = time.time()
    r = subprocess.run(['docker', 'exec', os.environ.get('JFS_GATE', 'snapgate'), 'bash', '-c', 'cd /w && ' + cmd],
                       capture_output=True, text=True, encoding='utf-8', errors='replace', timeout=timeout)
    return r.returncode, r.stdout + r.stderr, time.time() - t


def tests_in_tree():
    """package dir -> set of top-level Test functions, in the worktree."""
    res = {}
    for root, _, files in os.walk(tplace.WT):
        if '.git' in root:
            continue
        for f in files:
            if f.endswith('_test.go'):
                p = os.path.join(root, f)
                rel = os.path.relpath(root, tplace.WT).replace(os.sep, '/')
                for m in re.finditer(r'^func (Test\w+)\(t \*testing\.T\)', open(p, encoding='utf-8', errors='replace').read(), re.M):
                    res.setdefault(rel, set()).add(m.group(1))
    return res


def main(a, b):
    FINAL = json.load(open(os.path.join(HERE, 'final.json')))
    tplace.git('checkout', '-q', '-f', FINAL[a - 1] if a > 0 else '9325fe3')
    prev = tests_in_tree()
    for k in range(a, b + 1):
        tplace.git('checkout', '-q', '-f', FINAL[k])
        tplace.git('clean', '-qfd')
        cur = tests_in_tree()
        new = {p: sorted(cur[p] - (prev or {}).get(p, set())) for p in cur}
        new = {p: v for p, v in new.items() if v}
        prev = cur
        log = []
        ok = True
        steps = [('build', 'go build ./... 2>&1'), ('vet', 'go vet ./pkg/... ./cmd 2>&1'),
                 ('meta-core', "go test ./pkg/meta -run '^(TestSQLiteClient|TestMemKVClient)$' -count=1 -timeout 30m 2>&1")]
        for p, names in sorted(new.items()):
            if p.startswith('pkg/') or p == 'cmd':
                steps.append(('new:' + p, "go test ./%s -run '^(%s)$' -count=1 -timeout 30m -v 2>&1 | grep -v '<INFO>\\|<DEBUG>' " % (p, '|'.join(names))))
        summary = []
        for name, cmd in steps:
            rc, out, dt = dexec(cmd)
            if name.startswith('new:'):
                failed = re.findall(r'^--- FAIL: (\S+)', out, re.M) or ([] if re.search(r'^ok ', out, re.M) else ['?'])
                rc = 1 if failed else 0
            log.append('### %s rc=%d %.0fs\n%s' % (name, rc, dt, out[-20000:]))
            summary.append('%s=%s' % (name, 'ok' if rc == 0 else 'FAIL'))
            if rc != 0:
                ok = False
                if name in ('build', 'vet'):
                    break
        busy = 'device or resource busy' in '\n'.join(log)
        open(os.path.join(OUT, '%02d.log' % k), 'w', encoding='utf-8').write('\n'.join(log))
        print('%02d %s %s %s%s' % (k, 'PASS' if ok else 'FAIL', tplace.git('log', '-1', '--format=%s').strip()[:60],
                                    ' '.join(summary), ' EBUSY-SEEN' if busy else ''), flush=True)
        print('    new tests: %s' % {p: v for p, v in new.items()}, flush=True)


if __name__ == '__main__':
    main(int(sys.argv[1]), int(sys.argv[2]))
