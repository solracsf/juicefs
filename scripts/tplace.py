"""Place the target test declarations on the rebuilt code history, and apply
the per-commit code patches found by the build-all pass.

state.json keeps the placement (decl key -> commit index). Commands:
  gen K          write commit K's tree (code patches + tests) into the worktree WT
  compile A B    for K in A..B: gen + `go vet` in the gate container; a decl that
                 fails to compile is bumped to K+1, repeat until K compiles
Env: JFS_WT (worktree, default WORK/wt), JFS_GATE (container, default snapgate).
"""
import json, os, re, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from decls import decls

HERE = os.path.dirname(os.path.abspath(__file__))
TGT = os.path.join(HERE, 'tgt')
WT = os.environ.get('JFS_WT', os.path.join(HERE, 'wt'))
STATE = os.path.join(HERE, 'state.json')
NEW = json.load(open(os.path.join(HERE, 'newshas.json')))
ONE_INSTANT = 19

NEWFILES = ['pkg/meta/snapshot_freeze_test.go', 'pkg/meta/snapshot_clone_test.go', 'pkg/meta/snapshot_consistency_test.go',
            'pkg/meta/snapshot_compat_test.go', 'pkg/meta/snapshot_surface_test.go', 'pkg/fs/snapshot_freeze_test.go',
            'cmd/snapshot_test.go', 'pkg/meta/immutable_test.go', 'pkg/meta/openfile_test.go', 'pkg/meta/clone_test.go']
# upstream files the fixes append to: file -> decl names appended
APPEND = {'pkg/meta/utils_test.go': ['dropInode', 'TestEmptyDirDanglingEntry'],
          'pkg/fs/fs_test.go': ['TestReadOnlyHandleWrite']}


def git(*a, cwd=None):
    r = subprocess.run(['git', *a], capture_output=True, text=True, encoding='utf-8', cwd=cwd or WT)
    return r.stdout


def load_target():
    T = {}
    for f in NEWFILES + list(APPEND):
        h, imps, ds = decls(open(os.path.join(TGT, f), encoding='utf-8').read())
        if f in APPEND:
            ds = [d for d in ds if d[0] in APPEND[f]]
        T[f] = (h, imps, ds)
    return T


def owner(name):
    return name.split('.')[0]


def pkgname(imp):
    imp = imp.strip()
    if not imp or imp.startswith('//'):
        return None
    parts = imp.split()
    if len(parts) == 2:
        return parts[0]
    path = parts[0].strip('"').split('/')
    last = path[-1]
    if re.fullmatch(r'v\d+', last):
        last = path[-2]
    return last.replace('go-', '')


def strip_bool(text):
    """Drop the bestEffort argument of 6-argument CreateSnapshot calls (the
    signature before commit 19 has 5 arguments)."""
    out, i = [], 0
    key = 'CreateSnapshot('
    while True:
        j = text.find(key, i)
        if j < 0:
            out.append(text[i:])
            break
        out.append(text[i:j + len(key)])
        k = j + len(key)
        depth, args, start = 0, [], k
        while True:
            c = text[k]
            if c in '([{':
                depth += 1
            elif c in ')]}':
                if depth == 0:
                    args.append(text[start:k])
                    break
                depth -= 1
            elif c == ',' and depth == 0:
                args.append(text[start:k])
                start = k + 1
            k += 1
        if len(args) == 6:
            args = args[:3] + args[4:]
        out.append(','.join(args) + ')')
        i = k + 1
    return ''.join(out)


def closure(T, place):
    """A declaration is placed no earlier than the declarations it uses, and a
    type goes with its methods."""
    names = {}
    for f, (h, imps, ds) in T.items():
        for n, t in ds:
            names[f + '::' + n] = t
    changed = True
    while changed:
        changed = False
        byown = {}
        for key in names:
            f, n = key.split('::')
            byown.setdefault(f.split('/')[1] + '::' + owner(n), []).append(key)
        for keys in byown.values():
            m = max(place[k] for k in keys)
            for k in keys:
                if place[k] != m:
                    place[k] = m
                    changed = True
        for key, t in names.items():
            f, n = key.split('::')
            pkg = f.split('/')[1]
            body = '\n'.join(l for l in t.split('\n') if not l.lstrip().startswith('//'))
            for other in names:
                of, on = other.split('::')
                if of.split('/')[1] != pkg or other == key or '.' in on:
                    continue
                if re.search(r'\b%s\b' % re.escape(on), body) and place[other] > place[key]:
                    place[key] = place[other]
                    changed = True
    return place


def render(T, f, place, k, base=None):
    h, imps, ds = T[f]
    inc = [(n, t) for n, t in ds if place[f + '::' + n] <= k]
    if base is None and not inc:
        return None, []
    code = '\n\n'.join(t for _, t in inc)
    if k < ONE_INSTANT:
        code = strip_bool(code)
    if base is None:
        body = code
        used = [i for i in imps if pkgname(i) is None or re.search(r'\b%s\.' % re.escape(pkgname(i)), body)]
        lines = []
        for i in used:
            if i.strip() == '' and (not lines or lines[-1].strip() == ''):
                continue
            lines.append(i)
        while lines and lines[-1].strip() == '':
            lines.pop()
        text = h + 'import (\n' + '\n'.join(lines) + '\n)\n\n' + body + '\n'
    else:
        text = base
        if inc:
            text = base.rstrip('\n') + '\n\n' + code + '\n'
            bh, bimps, _ = decls(base)
            have = {pkgname(i) for i in bimps}
            need = [i for i in imps if pkgname(i) and pkgname(i) not in have and re.search(r'\b%s\.' % re.escape(pkgname(i)), code)]
            if need:
                text = text.replace('import (\n', 'import (\n' + ''.join(n + '\n' for n in need), 1)
    spans = []
    for n, t in inc:
        tt = strip_bool(t) if k < ONE_INSTANT else t
        pos = text.find(tt)
        if pos >= 0:
            a = text[:pos].count('\n') + 1
            spans.append((a, a + tt.count('\n'), n))
    return text, spans


def code_patches(k):
    """Fixes to intermediate commits found by building every commit:
    - commits 10..22 need the unicode import (the name check folded into
      commit 10 uses unicode.IsControl; the import only came with holds, 23);
    - commit 19's trusts check must use `differs` (c *snapshotCheck arrives
      with commit 20, which then rewrites the line);
    - before commit 19, CreateSnapshot takes 5 arguments in every test file."""
    p = os.path.join(WT, 'pkg/meta/base.go')
    s = open(p, encoding='utf-8', newline='').read()
    if 10 <= k <= 22:
        a = '\t"time"\n\n\taclAPI '
        assert s.count(a) == 1 and '\t"unicode"\n' not in s
        s = s.replace(a, '\t"time"\n\t"unicode"\n\n\taclAPI ')
    if k == 19:
        a = '!build.trusts(dst, sa) {\n\t\tc.differs.Store(true)\n'
        assert s.count(a) == 1
        s = s.replace(a, '!build.trusts(dst, sa) {\n\t\tdiffers.Store(true)\n')
    open(p, 'w', encoding='utf-8', newline='').write(s)
    if k < ONE_INSTANT:
        for root, _, files in os.walk(WT):
            if '.git' in root:
                continue
            for f in files:
                if f.endswith('_test.go'):
                    q = os.path.join(root, f)
                    t = open(q, encoding='utf-8', newline='').read()
                    if 'CreateSnapshot(' in t:
                        u = strip_bool(t)
                        if u != t:
                            open(q, 'w', encoding='utf-8', newline='').write(u)
                            print('  pre-19 signature in', os.path.relpath(q, WT))


def gen(k, T, place):
    git('checkout', '-q', '-f', NEW[k])
    git('clean', '-qfd', '--', 'pkg', 'cmd')
    code_patches(k)
    allspans = {}
    for f in NEWFILES:
        text, spans = render(T, f, place, k)
        p = os.path.join(WT, f)
        if text is None:
            if os.path.exists(p):
                os.remove(p)
            continue
        open(p, 'w', encoding='utf-8', newline='').write(text)
        subprocess.run(['gofmt', '-w', p])
        allspans[f] = spans
    for f in APPEND:
        p = os.path.join(WT, f)
        base = open(p, encoding='utf-8', newline='').read()
        text, spans = render(T, f, place, k, base=base)
        open(p, 'w', encoding='utf-8', newline='').write(text)
        subprocess.run(['gofmt', '-w', p])
        allspans[f] = spans
    return allspans


def vet():
    r = subprocess.run(['docker', 'exec', os.environ.get('JFS_GATE', 'snapgate'), 'sh', '-c', 'cd /w && go vet ./pkg/meta ./pkg/fs ./cmd 2>&1'],
                       capture_output=True, text=True, encoding='utf-8')
    return r.returncode, r.stdout


def compile_k(k, T, place):
    while True:
        place = closure(T, place)
        spans = gen(k, T, place)
        rc, out = vet()
        if rc == 0:
            return place, True
        bumped = set()
        for m in re.finditer(r'^(?:vet: )?((?:pkg|cmd)/[\w/]+_test\.go):(\d+):\d+: (.*)$', out, re.M):
            f, line, msg = m.group(1), int(m.group(2)), m.group(3)
            for a, b, n in spans.get(f, []):
                if a <= line <= b:
                    key = f + '::' + n
                    if place[key] <= k:
                        bumped.add(key)
        if not bumped:
            print(out[-3000:])
            return place, False
        for key in bumped:
            place[key] = k + 1
            print('  bump %s -> %d' % (key, k + 1))


if __name__ == '__main__':
    T = load_target()
    place = json.load(open(STATE))
    cmd = sys.argv[1]
    if cmd == 'gen':
        gen(int(sys.argv[2]), T, closure(T, place))
    elif cmd == 'compile':
        for k in range(int(sys.argv[2]), int(sys.argv[3]) + 1):
            place, ok = compile_k(k, T, place)
            json.dump(place, open(STATE, 'w'), indent=1)
            print('commit %d %s' % (k, 'compiles' if ok else 'FAILS'))
            if not ok:
                break
