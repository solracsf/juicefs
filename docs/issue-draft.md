Title: Read-only directory snapshots (`juicefs snapshot`)

**What would you like to be added**:

Read-only snapshots of a directory tree, kept under a hidden `/.snapshots` directory at the volume root, the way `/.trash` works today:

```
juicefs snapshot create  META-URL --path /data --name before-upgrade [--best-effort]
juicefs snapshot list    META-URL
juicefs snapshot delete  META-URL --name before-upgrade
juicefs snapshot hold    META-URL --name before-upgrade --tag backup-123
juicefs snapshot release META-URL --name before-upgrade --tag backup-123
juicefs config           META-URL --max-snapshots 30   # 0 = unlimited

ls /mnt/jfs/.snapshots/before-upgrade
juicefs clone /mnt/jfs/.snapshots/before-upgrade/config /mnt/jfs/config-restored
```

A snapshot copies metadata only. Every file in it points at the same slices as the original, and since JuiceFS never rewrites an object in place, the snapshot keeps serving what it captured after the originals change or are deleted. It costs inodes, not object storage.

I have a working implementation on a fork and would like to know whether you'd consider this direction before I open a PR. The short version of the design:

- **Eager copy, then validation.** None of the metadata engines has a cheap point-in-time view, so a snapshot is a copy of the tree made with the existing clone machinery, built detached (so `gc --delete` reaps a killed create) and attached under `/.snapshots` in one transaction. Before attaching, a second pass compares the copy with the live tree: entries, attributes including ctime to the nanosecond, xattrs, and which names are links to one file. A match means the copy is the tree at one instant between the two passes. Otherwise it retries, up to three times, then fails with `EBUSY`. `--best-effort` keeps the first copy without checking.
- **Directories and `SkipDirMtime`.** Sessions record their `SkipDirMtime`, and the comparison only trusts a directory listed at least that window after its own times, so a change hidden by skip-dir-mtime is not mistaken for no change.
- **Freeze.** A new `FlagSnapshot` plus `FlagImmutable` on every copied inode. Writes, namespace changes, attribute and xattr changes and atime updates are refused for everyone, root included. Compaction skips frozen inodes. Restore is a clone out of the snapshot.
- **Reserved inode range.** Snapshot roots come from a range above the trash range (`SnapshotInode = 0x7FFFFFFF20000000`) through a `nextSnapshot` counter; the trash range gains an upper bound. Content uses ordinary inodes, so gc's slice accounting is unchanged.
- **Holds**, like ZFS user holds: a hold is a reserved xattr on the snapshot root, and delete refuses with `EBUSY` while one exists. Each engine closes the hold/delete race in its own transaction (Redis WATCH, SQL row lock, KV entry rewrite), tested with two clients.
- **Accounting.** Snapshots are not charged to capacity or quotas (otherwise a volume more than half full could never snapshot itself); `snapshot list` reports their usage, and `--max-snapshots` bounds their number.
- **dump/load, gc, fsck.** The JSON dump gains a `Snapshots` section and the binary backup saves `nextSnapshot`. gc and the Redis leaked-inode sweep leave snapshots alone. `fsck --path / -r` also walks `/.snapshots`.

**Compatibility.** A client without snapshot support would ignore the freeze and its gc would delete snapshot roots, so the first snapshot moves the volume from metadata version 1 to 2. Older clients then refuse the volume at every command, and a snapshot is refused while a live session reports no support for version 2. Volumes that never take a snapshot stay at version 1 and are unaffected. One behaviour change applies everywhere: setting or removing an xattr now updates ctime, as on Linux filesystems, which the validation relies on.

**Known limits.** Creation is O(entries), and a tree that keeps changing may fail with `EBUSY`. Data a client has not committed yet is not in the snapshot. A change that restores every value and the same ctime to the nanosecond cannot be detected. atime is not captured as of the snapshot instant. There is no in-place rollback. Truly atomic, O(1) snapshots would need copy-on-write metadata in every engine, which this deliberately does not attempt.

**Upstream fixes found on the way.** Working on this turned up a few existing bugs, independent of snapshots, that I can send as separate PRs first if you prefer:

- `doWrite` does not refuse files with `FlagImmutable` (all engines).
- `File.Pwrite` in `pkg/fs` writes through a handle opened read-only.
- with `--open-cache`, a cached open skips the permission and immutable checks.
- Redis clone: a compaction between reading a source chunk and committing the clone can free slices the copy then references.
- KV clone: a slice held twice by one file gets its refcount raised once.
- Redis batch clone drops FIFOs, sockets and device nodes.
- SQL and KV batch clone fail the whole batch when one listed source was deleted meanwhile.
- `emptyDir` loops forever on an entry whose inode is gone.
- PostgreSQL `doSyncDirStat` fails on an existing row (already #7576).

It also needs `MemKV` reads to return copies; today they return the stored slice, which callers can modify in place.

**Why is this needed**:

Users want a cheap, consistent restore point before an upgrade, a migration or a risky job, and a stable source for backups and replication that does not move while it is read. Today the options are `juicefs clone` (writable, no consistency check, no protection against deletion) or a full `dump`. Every comparable filesystem has this (ZFS, Btrfs, CephFS, HDFS), and doing it at the metadata layer in JuiceFS costs no data copy.
