"""mktarget.py DIR: in a checkout of the pre-rebuild tip (454516d9, HANDOFF.md
removed), rewrite the test files into the target layout:
- the snapshot-free halves of the standalone fixes' tests move to upstream-style
  files (immutable_test.go, openfile_test.go, clone_test.go, utils_test.go,
  pkg/fs/fs_test.go);
- every test on Redis uses 127.0.0.1:6379: freeze/immutable DB 4, clone 5,
  consistency 6, compat 9;
- newTestMetas closes its sessions in t.Cleanup (TestRedisCache FlushAll's the
  server, and a leaked session's refresh then exits the test binary).
Run gofmt -w on the touched files afterwards."""
import re, sys, os

os.chdir(sys.argv[1])

HEADER = open('pkg/meta/snapshot_clone_test.go', encoding='utf-8').read().split('package meta')[0]


def rd(p):
    return open(p, encoding='utf-8', newline='').read()


def wr(p, s):
    open(p, 'w', encoding='utf-8', newline='').write(s)


def cut(s, start, end):
    """Remove s[start_marker .. end_marker) and return (rest, removed)."""
    i = s.index(start)
    j = s.index(end, i)
    return s[:i] + s[j:], s[i:j]


def sub1(s, a, b):
    assert s.count(a) == 1, (a, s.count(a))
    return s.replace(a, b)


# ---------------------------------------------------------------- freeze (meta)
p = 'pkg/meta/snapshot_freeze_test.go'
s = rd(p)
s, helper = cut(s, '// The tests here cover', '// snapshotFreezeTree builds')
s = sub1(s, 'import (\n\t"path"\n\t"strings"\n\t"syscall"', 'import (\n\t"syscall"')
s = s.replace('snapshotFreezeMetas(', 'newTestMetas(')
s = sub1(s, '// Write is refused on a snapshot file and on any immutable file, whether or not\n// the caller went through Open.\n',
         '// Write is refused on a snapshot file, whether or not the caller went through\n// Open.\n')
s, imm = cut(s, '\n\t\tvar live Ino\n\t\tif st := m.Create(ctx, RootInode, "live"', '\t}\n}\n\n// With the open cache on')
s = sub1(s, '// With the open cache on, a second open must still honor the freeze and the\n// permission bits, both of which the first open already checked.\n',
         '// With the open cache on, a second open must still honor the freeze, which the\n// first open already checked.\n')
s, priv = cut(s, '\n\t\tvar private Ino\n', '\t}\n}\n\n// Only CreateSnapshot')
wr(p, s)

helper = helper.split('\nconst snapshotFreezeRedis')[1]
helper = helper.split('\n', 2)[2]
helper = sub1(helper, 'func snapshotFreezeMetas(', 'func newTestMetas(')
helper = sub1(helper, '"freeze.db"', '"test.db"')
helper = sub1(helper, '"freeze-"+', '"test-"+')
helper = sub1(helper, 'newRedisMeta("redis", snapshotFreezeRedis, conf)', 'newRedisMeta("redis", "127.0.0.1:6379/4", conf)')
helper = sub1(helper, '''		if err := m.NewSession(true); err != nil {
			t.Fatalf("%s session: %s", name, err)
		}
	}
	return ms''', '''		if err := m.NewSession(true); err != nil {
			t.Fatalf("%s session: %s", name, err)
		}
		t.Cleanup(func() { _ = m.CloseSession() })
	}
	return ms''')
helper = ('// newTestMetas opens a fresh volume, with a session, on one engine of each\n'
          '// family: sqlite3, memkv, badger and Redis on 127.0.0.1:6379.\n') + helper
wr('pkg/meta/immutable_test.go', HEADER + 'package meta\n\nimport (\n\t"path"\n\t"strings"\n\t"syscall"\n\t"testing"\n\t"time"\n)\n\n' + helper +
   '// Write is refused on an immutable file, whether or not the caller went through\n'
   '// Open, and allowed again once the flag is cleared.\n'
   'func TestWriteImmutable(t *testing.T) {\n\tfor name, m := range newTestMetas(t, nil, nil) {\n\t\tctx := Background()' + imm + '\t}\n}\n')

priv = priv.replace('\n\t\tvar private Ino\n', '\t\tvar private Ino\n', 1)
wr('pkg/meta/openfile_test.go', HEADER + 'package meta\n\nimport (\n\t"syscall"\n\t"testing"\n\t"time"\n)\n\n'
   '// With the open cache on, a second open must still honor the permission bits,\n'
   '// which the first open already checked.\n'
   'func TestOpenCachePermissions(t *testing.T) {\n\tconf := testConfig()\n\tconf.OpenCache = time.Hour\n'
   '\tfor name, m := range newTestMetas(t, conf, nil) {\n\t\tuser := NewContext(1, 1000, []uint32{1000})\n\t\tvar attr Attr\n' + priv + '\t}\n}\n')

# ---------------------------------------------------------------- clone
p = 'pkg/meta/snapshot_clone_test.go'
s = rd(p)
head, rest = s.split('// The same deletion during a snapshot', 1)
head = sub1(head, '// cloneTestEngines holds one engine of each family; the redis one needs a\n// server on 127.0.0.1:6392.',
            '// cloneTestEngines holds one engine of each family; the redis one needs a\n// server on 127.0.0.1:6379.')
head = sub1(head, '"127.0.0.1:6392/1"', '"127.0.0.1:6379/5"')
wr('pkg/meta/clone_test.go', head.rstrip('\n') + '\n')
wr(p, HEADER + 'package meta\n\nimport (\n\t"sync"\n\t"syscall"\n\t"testing"\n)\n\n// The same deletion during a snapshot' + rest)

# ---------------------------------------------------------------- consistency
p = 'pkg/meta/snapshot_consistency_test.go'
s = rd(p)
s = sub1(s, '"127.0.0.1:6393/1"', '"127.0.0.1:6379/6"')
s, drop = cut(s, '// dropInode removes the attributes', '// TestSnapshotLeakedSweepVsBuild')
wr(p, s)

# ---------------------------------------------------------------- compat
p = 'pkg/meta/snapshot_compat_test.go'
wr(p, sub1(rd(p), '"redis://127.0.0.1:6394/1"', '"redis://127.0.0.1:6379/9"'))

# ---------------------------------------------------------------- utils
p = 'pkg/meta/utils_test.go'
s = rd(p)
s = sub1(s, 'import (\n\t"context"\n\t"testing"\n\t"time"\n)', 'import (\n\t"context"\n\t"syscall"\n\t"testing"\n\t"time"\n)')
s = s.rstrip('\n') + '\n\n' + drop.rstrip('\n') + '''

// Removing a tree with an entry whose inode is already gone fails instead of
// retrying forever.
func TestEmptyDirDanglingEntry(t *testing.T) {
	for name, m := range newTestMetas(t, nil, nil) {
		ctx := Background()
		var dir, sub Ino
		if st := m.Mkdir(ctx, RootInode, "d", 0755, 022, 0, &dir, &Attr{}); st != 0 {
			t.Fatalf("%s: mkdir d: %s", name, st)
		}
		if st := m.Mkdir(ctx, dir, "sub", 0755, 022, 0, &sub, &Attr{}); st != 0 {
			t.Fatalf("%s: mkdir sub: %s", name, st)
		}
		if st := m.Create(ctx, sub, "f", 0644, 022, 0, new(Ino), &Attr{}); st != 0 {
			t.Fatalf("%s: create f: %s", name, st)
		}
		dropInode(t, m, sub)
		done := make(chan syscall.Errno, 1)
		go func() { done <- m.Remove(ctx, RootInode, "d", true, RmrDefaultThreads, nil) }()
		select {
		case st := <-done:
			if st == 0 {
				t.Errorf("%s: removing a tree with a dangling entry succeeded", name)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: removing a tree with a dangling entry did not return", name)
		}
	}
}
'''
wr(p, s)

# ---------------------------------------------------------------- fs
p = 'pkg/fs/snapshot_freeze_test.go'
s = rd(p)
s, live = cut(s, '\n\n\t// handles with write access keep working\n', '\n}\n')
wr(p, s)
live = live.replace('"/src/f"', '"/f"')
p = 'pkg/fs/fs_test.go'
s = rd(p).rstrip('\n') + '''

// A handle opened without write access cannot write, while handles with write
// access can.
func TestReadOnlyHandleWrite(t *testing.T) {
	fs := createTestFS(t)
	ctx := meta.NewContext(1, 1000, []uint32{1000})
	f, e := fs.Create(ctx, "/f", 0644, 0)
	if e != 0 {
		t.Fatal(e)
	}
	if _, e := f.Write(ctx, []byte("original")); e != 0 {
		t.Fatal(e)
	}
	if e := f.Close(ctx); e != 0 {
		t.Fatal(e)
	}
	r, e := fs.Open(ctx, "/f", vfs.MODE_MASK_R)
	if e != 0 {
		t.Fatal(e)
	}
	if n, e := r.Pwrite(ctx, []byte("TAMPERED"), 0); e != syscall.EBADF || n != 0 {
		t.Errorf("pwrite through a read-only handle: (%d, %v)", n, e)
	}
	if n, e := r.Write(ctx, []byte("TAMPERED")); e != syscall.EBADF || n != 0 {
		t.Errorf("write through a read-only handle: (%d, %v)", n, e)
	}
	if e := r.Flush(ctx); e != 0 {
		t.Errorf("flush: %v", e)
	}
	_ = r.Close(ctx)
	r, e = fs.Open(ctx, "/f", vfs.MODE_MASK_R)
	if e != 0 {
		t.Fatal(e)
	}
	buf := make([]byte, 8)
	if _, err := r.Pread(ctx, buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "original" {
		t.Errorf("file changed through a read-only handle: %q", buf)
	}
	_ = r.Close(ctx)
''' + live + '\n}\n'
wr(p, s)
print('ok')
