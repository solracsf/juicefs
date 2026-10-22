"""carry_apply.py NNN.json ... : apply carried hunks to the working tree, gofmt,
git add, and rename the carry to .done. Then: git commit --amend --no-edit."""
import json, os, re, subprocess, sys
HERE = os.path.dirname(os.path.abspath(__file__))
def norm(s): return re.sub(r'\s+', '', s)
for name in sys.argv[1:]:
    p = os.path.join(HERE, 'carry', name)
    c = json.load(open(p, encoding='utf-8')); h = [tuple(x) for x in c['hunk']]
    lines = open(c['file'], encoding='utf-8', newline='').read().split('\n')
    pre = [x for t, x in h if t != '+']; k = [norm(x) for x in pre]
    m = [i for i in range(len(lines)-len(pre)+1) if all(norm(lines[i+j]) == k[j] for j in range(len(pre)))]
    assert len(m) == 1, m
    s = m[0]; seg = lines[s:s+len(pre)]; post = []; q = 0
    for t, x in h:
        if t == ' ': post.append(seg[q]); q += 1
        elif t == '-': q += 1
        else: post.append(x)
    lines = lines[:s] + post + lines[s+len(pre):]
    open(c['file'], 'w', encoding='utf-8', newline='').write('\n'.join(lines))
    if c['file'].endswith('.go'): subprocess.run(['gofmt', '-w', c['file']])
    subprocess.run(['git', 'add', c['file']])
    os.rename(p, p + '.done')
    print('applied', name, 'to', c['file'])
