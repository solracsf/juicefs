"""Assemble the final history from new2 + code patches + placed tests, with the
messages in msgs/NN.txt where present (else the new2 message). Writes
final.json and moves branch `final`. Author dates are kept; author and
committer are Git'Fellow. Run with JFS_WT pointing at a worktree no gate is
using; detach any worktree that has `final` checked out first."""
import json, os, re, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tplace

HERE = os.path.dirname(os.path.abspath(__file__))
SIGN = "Signed-off-by: Git'Fellow <12234510+solracsf@users.noreply.github.com>"
ME = {'GIT_AUTHOR_NAME': "Git'Fellow", 'GIT_AUTHOR_EMAIL': '12234510+solracsf@users.noreply.github.com',
      'GIT_COMMITTER_NAME': "Git'Fellow", 'GIT_COMMITTER_EMAIL': '12234510+solracsf@users.noreply.github.com'}


def g(*a, env=None, inp=None):
    e = dict(os.environ)
    e.update(env or {})
    r = subprocess.run(['git', *a], capture_output=True, text=True, encoding='utf-8', cwd=tplace.WT, env=e, input=inp)
    if r.returncode:
        raise SystemExit('git %s: %s' % (a, r.stderr))
    return r.stdout


T = tplace.load_target()
place = tplace.closure(T, json.load(open(tplace.STATE)))
parent = g('rev-parse', '9325fe3').strip()
final = []
for k, sha in enumerate(tplace.NEW):
    tplace.gen(k, T, place)
    g('add', '-A')
    tree = g('write-tree').strip()
    mp = os.path.join(HERE, 'msgs', '%02d.txt' % k)
    msg = open(mp, encoding='utf-8').read() if os.path.exists(mp) else g('log', '-1', '--format=%B', sha)
    msg = msg.rstrip('\n') + '\n'
    assert msg.rstrip('\n').split('\n')[-1] == SIGN, (k, msg[-200:])
    assert not re.search(r'co-authored-by|assisted-by|generated with|claude|anthropic', msg, re.I), k
    adate = g('log', '-1', '--format=%aI', sha).strip()
    c = g('commit-tree', tree, '-p', parent, env=dict(ME, GIT_AUTHOR_DATE=adate), inp=msg).strip()
    final.append(c)
    parent = c
    print(k, c[:8], msg.split('\n')[0])
json.dump(final, open(os.path.join(HERE, 'final.json'), 'w'))
g('branch', '-f', 'final', parent)
print('final', parent)
