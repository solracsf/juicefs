"""Resolution of the 'preserve hard links in snapshots' pick (commit 20): merge
the original snapshotLinks into snapshotBuild, the final form. Run during the
conflicted pick, then insert testSnapshotHardlinks in base_test.go before the
'// Compacting a file merges' comment:
  HK_U=1 python ../hk.py apply pkg/meta/base_test.go '0,1@// Compacting a file merges its slices+-1'
If r19 wrote `differs.Store(true)` in the trusts check, the f19ba5e4 fixup
right after turns it into c.differs."""
import subprocess
p = 'pkg/meta/base.go'
s = subprocess.run(['git', 'show', ':2:' + p], capture_output=True, text=True, encoding='utf-8').stdout


def rep(a, b):
    global s
    assert s.count(a) == 1, (a, s.count(a))
    s = s.replace(a, b)


rep('\tdoGetXattrs(ctx Context, inode Ino) (map[string][]byte, syscall.Errno)\n',
    '''	doGetXattrs(ctx Context, inode Ino) (map[string][]byte, syscall.Errno)
	// doSnapshotLink adds parent/name as another link to inode, a file already
	// copied into a snapshot under construction; flags and times are left alone
	doSnapshotLink(ctx Context, inode, parent Ino, name string) syscall.Errno
''')
rep('\tbuild := &snapshotBuild{listed: make(map[Ino]time.Time)}\n',
    '\tbuild := &snapshotBuild{copies: make(map[Ino]*snapshotCopy), listed: make(map[Ino]time.Time)}\n')
rep('\t\tvar nonDirEntries []*Entry\n', '\t\tvar nonDirEntries, linked []*Entry\n')
rep('''				}
			} else {
				nonDirEntries = append(nonDirEntries, e)
			}''', '''				}
			} else if build != nil && e.Attr.Nlink > 1 {
				linked = append(linked, e)
			} else {
				nonDirEntries = append(nonDirEntries, e)
			}''')
rep('''			eno = m.BatchClone(cloneCtx, srcIno, ino, nonDirEntries, cmode, cumask, count)
			if eno != 0 {
				break
			}
		}
''', '''			eno = m.BatchClone(cloneCtx, srcIno, ino, nonDirEntries, cmode, cumask, count)
			if eno != 0 {
				break
			}
		}
		for _, e := range linked {
			if eno = m.cloneLinked(cloneCtx, build, e, ino, cmode, cumask, count); eno != 0 {
				break
			}
		}
		if eno != 0 {
			break
		}
''')
rep('''// snapshotBuild is what one snapshot copy keeps while it is being built: for a
// consistent build, when each directory's entries were listed for copying and
// how long a change to them could go unnoticed without moving their mtime or
// ctime.
type snapshotBuild struct {
	sync.Mutex
	listed map[Ino]time.Time // by the inode of the copy
	window time.Duration     // 0 for a best-effort build, which never checks it
}
''', '''// snapshotBuild is what one snapshot copy keeps while it is being built: each
// hard-linked file of the tree mapped to its copy, so every name of the file
// in the tree ends up as a link to one copy; and, for a consistent build, when
// each directory's entries were listed for copying and how long a change to
// them could go unnoticed without moving their mtime or ctime.
type snapshotBuild struct {
	sync.Mutex
	copies map[Ino]*snapshotCopy
	listed map[Ino]time.Time // by the inode of the copy
	window time.Duration     // 0 for a best-effort build, which never checks it
}

type snapshotCopy struct {
	done chan struct{}
	ino  Ino
	st   syscall.Errno
}
''')
rep('''	return ok && !time.Unix(attr.Mtime, int64(attr.Mtimensec)).Add(b.window).After(listed)
}
''', '''	return ok && !time.Unix(attr.Mtime, int64(attr.Mtimensec)).Add(b.window).After(listed)
}

// cloneLinked copies e, a file with several links, the first time the snapshot
// meets it, and links the copy under every other name it has in the tree.
func (m *baseMeta) cloneLinked(ctx Context, build *snapshotBuild, e *Entry, parent Ino, cmode uint8, cumask uint16, count *uint64) syscall.Errno {
	build.Lock()
	c, seen := build.copies[e.Inode]
	if !seen {
		c = &snapshotCopy{done: make(chan struct{})}
		build.copies[e.Inode] = c
	}
	build.Unlock()
	if !seen {
		c.st = m.cloneEntry(ctx, e.Inode, parent, string(e.Name), &c.ino, cmode, cumask, count, false, nil)
		close(c.done)
	} else {
		<-c.done
		if c.st == 0 {
			if st := m.en.doSnapshotLink(ctx, c.ino, parent, string(e.Name)); st != 0 {
				return st
			}
			atomic.AddUint64(count, 1)
		}
	}
	if c.st == syscall.ENOENT {
		logger.Warnf("ignore deleted %s in dir %d", string(e.Name), parent)
		return 0
	}
	return c.st
}
''')
open(p, 'w', encoding='utf-8', newline='').write(s)
print('ok')
