"""Resolution at commit 19 ('take snapshots at one instant') of fixup e91ce77e
(h0; db1ca3aa in the integration branch): only the directory-mtime window.
snapshotBuild holds listed/window only; the hard-link parts (copies,
cloneLinked) come with commit 20 (r20.py). Run in the work clone during the
conflicted fixup, then apply sql/tkv hunks with `HK_U=1 hk.py apply`, gofmt,
check with msdiff.sh. NOTE: the trusts check below writes c.differs, which is
wrong at commit 19 (it has `differs *atomic.Bool`); tplace.code_patches fixes
commit 19's tree to `differs.Store(true)` at assembly. Writing `differs` here
directly is better, but then commit 20's resolution must turn it into c.differs."""
import subprocess
p = 'pkg/meta/base.go'
s = subprocess.run(['git', 'show', ':2:' + p], capture_output=True, text=True, encoding='utf-8').stdout


def rep(a, b):
    global s
    assert s.count(a) == 1, (a, s.count(a))
    s = s.replace(a, b)


rep('\tdoRepair(ctx Context, inode Ino, attr *Attr, trustNlink bool) syscall.Errno\n',
    '''	// dirMtimeWindow turns skip, a configured SkipDirMtime, into how long after a
	// directory's mtime was set a change to its entries might still leave the
	// mtime and ctime alone on this engine, which may apply skip differently
	dirMtimeWindow(skip time.Duration) time.Duration
	doRepair(ctx Context, inode Ino, attr *Attr, trustNlink bool) syscall.Errno
''')
rep('\t\tProcessID:   os.Getpid(),\n\t})', '\t\tProcessID:   os.Getpid(),\n\t\tSkipDirMtime: m.conf.SkipDirMtime,\n\t})')
rep('''// copy is the state of src at one instant between the two passes.
func (m *baseMeta) buildSnapshot(''', '''// copy is the state of src at one instant between the two passes -- except for
// a directory that SkipDirMtime let change without moving its mtime or ctime;
// snapshotMatches distrusts those instead of trusting stale-looking attributes
// (see (*snapshotBuild).trusts).
func (m *baseMeta) buildSnapshot(''')
rep('''	dst := root
	st := m.cloneEntry(ctx, src, SnapshotInode, name, &dst, cmode, 0, count, true, make(chan struct{}, CLONE_DEFAULT_CONCURRENCY))
	if st == 0 && consistent {
		var same bool''', '''	dst := root
	build := &snapshotBuild{listed: make(map[Ino]time.Time)}
	if consistent {
		if build.window, err = m.dirMtimeWindow(); err != nil {
			return 0, errno(err)
		}
	}
	ctx = ctx.WithValue(snapshotBuildKey{}, build)
	st := m.cloneEntry(ctx, src, SnapshotInode, name, &dst, cmode, 0, count, true, make(chan struct{}, CLONE_DEFAULT_CONCURRENCY))
	if st == 0 && consistent {
		// a client that mounted since window was first measured runs its own
		// SkipDirMtime, which the sessions above may not have counted yet
		if window, err := m.dirMtimeWindow(); err != nil {
			return root, errno(err)
		} else if window > build.window {
			build.window = window
		}
		var same bool''')
rep('''	return root, st
}

// snapshotMatches reports''', '''	return root, st
}

// dirMtimeWindow is the longest span, across every session on the volume this
// one included, during which a change to a directory's entries might leave its
// mtime and ctime alone: each client may run with its own SkipDirMtime, and
// engines may apply it differently (see engine.dirMtimeWindow).
func (m *baseMeta) dirMtimeWindow() (time.Duration, error) {
	sessions, err := m.en.ListSessions()
	if err != nil {
		return 0, err
	}
	skip := m.conf.SkipDirMtime
	for _, s := range sessions {
		skip = max(skip, s.SkipDirMtime)
	}
	return m.en.dirMtimeWindow(skip), nil
}

// snapshotMatches reports''')
rep('''// latter only count the links a snapshot copies.
func (m *baseMeta) snapshotMatches(''', '''// latter only count the links a snapshot copies.
//
// Matching mtime and ctime do not by themselves prove a directory's entries
// are unchanged: SkipDirMtime lets any client leave them alone for a while
// after touching them. (*snapshotBuild).trusts only credits a directory whose
// entries were listed for the copy at least a window past those times, since a
// change landing after such a listing is then guaranteed to move them; one
// listed sooner is treated as changed, which fails the comparison and sends
// CreateSnapshot's caller back to retry.
func (m *baseMeta) snapshotMatches(''')
rep('''
	// the copy is frozen, so hold its entries and walk the live side against them''', '''	if build, _ := ctx.Value(snapshotBuildKey{}).(*snapshotBuild); build != nil && !build.trusts(dst, sa) {
		c.differs.Store(true)
		return 0
	}

	// the copy is frozen, so hold its entries and walk the live side against them''')
rep('''	// Use DirHandler for batch processing to avoid loading all entries at once''', '''	build, _ := ctx.Value(snapshotBuildKey{}).(*snapshotBuild)
	if build != nil {
		// mark the moment we are about to read this directory's entries, before
		// NewDirHandler lists them, so snapshotMatches can later tell whether
		// SkipDirMtime could still be hiding a later change from that listing
		build.listing(ino)
	}
	// Use DirHandler for batch processing to avoid loading all entries at once''')
rep('''	return eno
}

func (m *baseMeta) mergeAttr(''', '''	return eno
}

type snapshotBuildKey struct{}

// snapshotBuild is what one snapshot copy keeps while it is being built: for a
// consistent build, when each directory's entries were listed for copying and
// how long a change to them could go unnoticed without moving their mtime or
// ctime.
type snapshotBuild struct {
	sync.Mutex
	listed map[Ino]time.Time // by the inode of the copy
	window time.Duration     // 0 for a best-effort build, which never checks it
}

// listing records that the entries of the directory copied as dst are about to
// be read.
func (b *snapshotBuild) listing(dst Ino) {
	b.Lock()
	b.listed[dst] = time.Now()
	b.Unlock()
}

// trusts tells whether a live directory found to share dst's mtime and ctime
// can be trusted not to have gained or lost entries since: only once dst's
// entries were listed, and only if that listing came window or more after
// those times, is any later change to the directory certain to have moved
// them (see snapshotMatches). A directory never listed, or listed too soon
// after its own times were set, is not trusted.
func (b *snapshotBuild) trusts(dst Ino, attr *Attr) bool {
	b.Lock()
	listed, ok := b.listed[dst]
	b.Unlock()
	return ok && !time.Unix(attr.Mtime, int64(attr.Mtimensec)).Add(b.window).After(listed)
}

func (m *baseMeta) mergeAttr(''')
open(p, 'w', encoding='utf-8', newline='').write(s)
print('ok')
