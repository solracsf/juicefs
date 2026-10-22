"""Carried hunks: carry/<n>.json = {file, hunk:[[tag,text],...], note}.
check (run by the rebase todo after every step): exit 1 if any carried hunk's
full pre-image now exists (whitespace-insensitive) in the working tree, so the
rebase stops for it. Apply with carry_apply.py, then amend and continue."""
import json, os, re, sys, glob
HERE = os.path.dirname(os.path.abspath(__file__))
def norm(s): return re.sub(r'\s+', '', s)
def pre(h): return [x for t, x in h if t != '+']
def found(lines, p):
    n = len(p); k = [norm(x) for x in p]
    return [i for i in range(len(lines)-n+1) if all(norm(lines[i+j]) == k[j] for j in range(n))]
ready = []
for f in sorted(glob.glob(os.path.join(HERE, 'carry', '*.json'))):
    c = json.load(open(f, encoding='utf-8'))
    if not os.path.exists(c['file']): continue
    lines = open(c['file'], encoding='utf-8').read().split('\n')
    m = found(lines, pre(c['hunk']))
    if m: ready.append((os.path.basename(f), c['file'], m, c.get('note','')))
for r in ready: print('CARRY READY', *r)
sys.exit(1 if ready else 0)
