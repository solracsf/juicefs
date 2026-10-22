"""Resolution of the 'hold snapshots against deletion' pick (commit 23). Run
during the conflicted pick from the work clone (needs ../hk.py). Flags follow
the per-subcommand layout made at commit 10: holdTagFlag() helper, and
hold/release get Flags {snapshotNameFlag(), holdTagFlag()}."""
import subprocess, re, os

HK = ['python', '../hk.py']


def run(*a, env=None):
    e = dict(os.environ)
    if env:
        e.update(env)
    r = subprocess.run(list(a), capture_output=True, text=True, encoding='utf-8', env=e)
    assert r.returncode == 0, r.stderr
    return r.stdout


def plus(f, i):
    d = run('git', 'diff', '-U3', 'REBASE_HEAD^', 'REBASE_HEAD', '--', f)
    hs = []
    cur = None
    for l in d.split('\n'):
        if l.startswith('@@'):
            cur = []
            hs.append(cur)
        elif cur is not None and l.startswith('+'):
            cur.append(l[1:])
    return hs[i]


def rd(p):
    return open(p, encoding='utf-8', newline='').read()


def wr(p, s):
    open(p, 'w', encoding='utf-8', newline='').write(s)


def rep(s, a, b):
    assert s.count(a) == 1, (a, s.count(a))
    return s.replace(a, b)


def before_func_comment(s, funcline, block):
    lines = s.split('\n')
    k = [i for i, l in enumerate(lines) if l.startswith(funcline)]
    assert len(k) == 1, funcline
    i = k[0]
    while lines[i - 1].startswith('//'):
        i -= 1
    return '\n'.join(lines[:i] + block + lines[i:])


# cmd/snapshot.go
f = 'cmd/snapshot.go'
run(*HK, 'apply', f, '0,1,2,5,6', env={'HK_U': '3'})
s = rd(f)
sub = plus(f, 4)
sub = '\n'.join(sub)
sub = sub.replace('\t\t\t\tAction:    func(c *cli.Context) error { return holdOrRelease(c, true) },\n',
                  '\t\t\t\tAction:    func(c *cli.Context) error { return holdOrRelease(c, true) },\n\t\t\t\tFlags:     []cli.Flag{snapshotNameFlag(), holdTagFlag()},\n')
sub = sub.replace('\t\t\t\tAction:    func(c *cli.Context) error { return holdOrRelease(c, false) },\n',
                  '\t\t\t\tAction:    func(c *cli.Context) error { return holdOrRelease(c, false) },\n\t\t\t\tFlags:     []cli.Flag{snapshotNameFlag(), holdTagFlag()},\n')
assert sub.count('holdTagFlag()') == 2, sub
s = rep(s, '''				Action:    snapshotDelete,
				Flags:     []cli.Flag{snapshotNameFlag()},
			},
''', '''				Action:    snapshotDelete,
				Flags:     []cli.Flag{snapshotNameFlag()},
			},
''' + sub + '\n')
s = rep(s, '''func snapshotNameFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "name",
		Usage: "name of the snapshot",
	}
}
''', '''func snapshotNameFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "name",
		Usage: "name of the snapshot",
	}
}

func holdTagFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "tag",
		Usage: "name of the hold",
	}
}
''')
tail = plus(f, 7)
ebusy, rest = tail[:3], tail[3:]
s = rep(s, '''	spin.Done()
	progress.Done()
	if st != 0 {
		return fmt.Errorf("delete snapshot %s: %s", name, st)
	}''', '''	spin.Done()
	progress.Done()
''' + '\n'.join(ebusy) + '''
	if st != 0 {
		return fmt.Errorf("delete snapshot %s: %s", name, st)
	}''')
s = s.rstrip('\n') + '\n' + '\n'.join(rest).rstrip('\n') + '\n'
wr(f, s)

# pkg/meta/base.go
f = 'pkg/meta/base.go'
run(*HK, 'apply', f, '0,2,3,5,7,8,9', env={'HK_U': '3'})
s = rd(f)
s = rep(s, '\tdoSnapshotLink(ctx Context, inode, parent Ino, name string) syscall.Errno\n',
        '\tdoSnapshotLink(ctx Context, inode, parent Ino, name string) syscall.Errno\n' + '\n'.join(plus(f, 1)) + '\n')
s = rep(s, '\tCreated time.Time\n}\n', '\tCreated time.Time\n' + '\n'.join(plus(f, 4)) + '\n}\n')
blk = plus(f, 6)
lines = s.split('\n')
k = [i for i, l in enumerate(lines) if l == '\tif len(sx) != len(dx) {']
assert len(k) == 1
lines[k[0]:k[0]] = blk
s = '\n'.join(lines)
wr(f, s)

# base_test.go
f = 'pkg/meta/base_test.go'
run(*HK, 'apply', f, '0,1,2@// Compacting a file merges its slices+-1', env={'HK_U': '3'})

# engines
for f, typ in (('pkg/meta/sql.go', 'dbMeta'), ('pkg/meta/tkv.go', 'kvMeta')):
    run(*HK, 'apply', f, '0,1,2', env={'HK_U': '3'})
    s = rd(f)
    blk = plus(f, 3)
    s = before_func_comment(s, 'func (m *%s) dirMtimeWindow(' % typ, blk)
    wr(f, s)
for f in ('cmd/snapshot.go', 'pkg/meta/base.go', 'pkg/meta/base_test.go', 'pkg/meta/sql.go', 'pkg/meta/tkv.go'):
    subprocess.run(['gofmt', '-w', f])
print('ok')
