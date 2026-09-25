---
title: Snapshots
sidebar_position: 7
description: Learn how to take read-only, metadata-only snapshots of JuiceFS directories, and how to list, read, restore from and delete them.
---

A snapshot is a read-only copy of a directory tree as it was at one instant, kept under the hidden `/.snapshots` directory of the volume. Like [clone](./clone.md), a snapshot copies metadata only: the files in a snapshot reference the same object storage blocks as the originals, so taking a snapshot is fast and does not duplicate any data, whatever the size of the tree.

Unlike a clone, a snapshot is frozen. Nobody can write to it, delete from it, rename anything inside it or change the attributes or extended attributes of what it contains, not even root. When the original files are later modified or removed, the snapshot keeps serving the data it captured, because JuiceFS never overwrites an object storage block in place.

Snapshots are available from JuiceFS 1.5.

## Create, list and delete {#usage}

Snapshots are managed with the [`juicefs snapshot`](../reference/command_reference.mdx#snapshot) command, which talks to the metadata engine directly:

```shell
# Snapshot the whole volume
juicefs snapshot create redis://localhost --path / --name daily-2026-09-23

# Snapshot one directory
juicefs snapshot create redis://localhost --path /data --name before-upgrade

# List them, with the inodes and size each one holds
juicefs snapshot list redis://localhost

# Delete one
juicefs snapshot delete redis://localhost --name before-upgrade
```

`--path` is relative to the root of the volume. A snapshot name must be unique within the volume.

Snapshots are taken of live files only. A directory that belongs to a snapshot, `/.snapshots` itself included, cannot be the source of another snapshot, whatever path leads to it: the check is made on the inode, not on the path. To snapshot what a snapshot holds, [clone](./clone.md) it out of the snapshot first.

## Holds {#holds}

A hold keeps a snapshot from being deleted until it is released, like an OpenZFS [user hold](https://openzfs.github.io/openzfs-docs/man/master/8/zfs-hold.8.html), so that a backup, a transfer or any other job reading from a snapshot cannot lose it halfway. Each hold has a tag of your choice, 1 to 200 bytes of UTF-8 without control characters; a snapshot can have several, each one released on its own, and `juicefs snapshot delete` fails as long as any is left:

```shell
juicefs snapshot hold redis://localhost --name before-upgrade --tag backup-123
juicefs snapshot delete redis://localhost --name before-upgrade   # fails: the snapshot is held
juicefs snapshot release redis://localhost --name before-upgrade --tag backup-123
```

`juicefs snapshot list` shows the holds of each snapshot. Holds are kept in the metadata engine, on the root of the snapshot, so they survive client restarts and crashes, `juicefs gc`, and `juicefs dump` and `juicefs load`. The check that a snapshot has no hold and its deletion happen in the same metadata transaction, so a hold taken while the snapshot is being deleted either stops the deletion or fails because the snapshot is gone. A hold is not part of the snapshot: taking or releasing one changes neither the attributes of the snapshot root, its ctime included, nor the extended attributes it lists. Extended attribute names starting with `juicefs.snapshot.hold.` are reserved for holds and cannot be read, set or removed through a mount, and copies made from a held snapshot are not held.

## Read and restore {#read-and-restore}

On a mount point, a snapshot appears as `/.snapshots/<name>` and can be read like any other directory. `.snapshots` is listed in the root of the volume once the first snapshot exists, the same way as `.trash`: the `--hide-internal` mount option hides it from listings, and `--prefix-internal` renames it to `.jfs.snapshots`. It is not visible on a mount of a subdirectory. A mount sees snapshots that `juicefs snapshot` creates or deletes as it sees any change made by another client, once its entry and attribute caches expire (1 second by default); until then, a snapshot created under the name of one just deleted may not be readable through that mount.

To restore files, copy them out of the snapshot. [`juicefs clone`](./clone.md) does it without copying any data, and the copy is an ordinary, writable file or directory:

```shell
juicefs clone /mnt/jfs/.snapshots/before-upgrade/config /mnt/jfs/config-restored
```

## Consistency {#consistency}

- A snapshot becomes visible only once it is complete. A snapshot that fails is removed again at once; one whose creation was killed is removed by a later [`juicefs gc --delete`](../reference/command_reference.mdx#gc).
- By default, a snapshot is published only once JuiceFS has validated that the tree did not change while it was copied: the tree is copied, the copy is compared with the live tree, and the snapshot is published only if they match. If anything changed, the copy is discarded and taken again, up to three times, and `juicefs snapshot create` fails with "device or resource busy" rather than publish a mixed tree. Take the snapshot when the tree is quieter, or stop writing to it while it is taken.
- The comparison covers the namespace, file contents, inode attributes other than the access time, and extended attributes. It uses ctime as its change signal: every change a client makes sets the ctime of the file or directory it touches to the current time, so even a change undone before the comparison, such as a file renamed away and back, is seen. ctime is a timestamp taken from the writing client's clock, not a version counter, so a change that ends with the same values and the very same ctime as before, to the nanosecond, would not be seen. The comparison reads the live metadata, not a historical view of the metadata engine.
- Access times are deliberately outside the comparison, since reads move them: the access time of an entry in a snapshot is the one its source had when it was copied, which may not match the other entries' instant, and reading the snapshot never updates it.
- What validation proves is therefore limited to that scope: a published snapshot matches the tree, as JuiceFS's metadata holds it, in its namespace, file contents, attributes other than access times and extended attributes, at one instant between the copy and the comparison, barring a change that leaves the same ctime to the nanosecond. It says nothing about data not yet committed to the metadata engine, described below. This differs from OpenZFS, where a snapshot is taken atomically at a storage transaction boundary and includes every system call that completed before it.
- With `--best-effort`, the first copy is kept whatever changed during it, and nothing is validated. Such a snapshot is not a point-in-time image: it can keep a change and miss an earlier one it depends on, and a file renamed during the copy can be missing from it or appear in it twice, under both names. Do not rely on any relation between its entries, nor between the contents, attributes and extended attributes of a file that changed during the copy.
- A snapshot captures what the metadata engine holds. Data that a client has written but not yet committed is not part of it: a client commits written data when the file is flushed or closed, or a few seconds after the write. Unlike a ZFS snapshot, which includes every write that returned, a snapshot of a file being written holds its content as of the last commit.
- A client mounted with `--writeback` commits data before uploading it. Until the upload finishes, the blocks exist only in that client's cache, and a snapshot taken in the meantime depends on them as much as the original file does: if the cache is lost, the data is lost from both.
- Hard links are preserved: the names a file has inside the snapshotted tree are links to one copy, and its link count in the snapshot counts only those names.
- Symbolic links are copied verbatim, target included. A relative link resolves inside the snapshot, but an absolute one resolves as it would anywhere else, so it can point out of the snapshot, into the live tree or outside the volume.

## Space and accounting {#space}

A snapshot does not copy data, but it does copy metadata: every file and directory in the snapshot is an inode in the metadata engine, so a snapshot of a tree with a million files adds a million inodes and the matching metadata storage.

Snapshots are not counted against the capacity or inode limits of the volume, nor against directory, user or group quotas, and `df` does not include them: snapshot metadata bypasses the inode and quota accounting altogether, so none of those limits stops a snapshot from being taken. Their usage is reported by `juicefs snapshot list` instead, as the number of inodes and the logical size of each snapshot. That size is the size of the files the snapshot holds; since those files share their blocks with the originals and with other snapshots, it is not the space that deleting the snapshot would free.

While a snapshot exists, the blocks it references stay in object storage even after the original files are deleted. Deleting the snapshot removes its metadata and releases the blocks that no other file or snapshot references, before the command returns.

The only built-in bound on snapshots is their number. To limit how much metadata snapshots can add, set how many snapshots a volume may hold:

```shell
juicefs config redis://localhost --max-snapshots 30
```

Once the limit is reached, `juicefs snapshot create` fails until a snapshot is deleted. The default, 0, means no limit. The limit counts snapshots, not their size: a snapshot of a large tree still adds one inode per file and directory it holds.

## Compatibility {#compatibility}

- A volume is formatted, and stays, at metadata version 1 until its first snapshot: that is the version every client before 1.5 formats and supports, so a volume that never takes a snapshot works with older clients exactly as before. The first attempt to create a snapshot raises the volume to metadata version 2, since a client that does not support it would neither honor the freeze on a snapshot nor spare its inodes in `juicefs gc`. The attempt raises it before anything else, so it stays raised even if that snapshot then fails, for example because an older client is still mounted; the client logs a warning when it raises the version, and a `juicefs config` or `juicefs format` racing the raise never lowers it back. From then on, every client checks the metadata version of the volume against the highest one it supports at every command, including `mount`, `gc`, `dump`, `load` and a `config` that changes anything, and refuses it if it is too high, whatever the client's release; a client already mounted leaves on its own at its next refresh. A snapshot is itself refused while a client that does not report support for metadata version 2 in its session, which includes every client from before 1.5, is still mounted, so that one cannot go on serving a frozen tree it does not understand; wait for it to leave or unmount it.
- If the volume already has a directory called `/.snapshots` at its root, it stays accessible, but snapshots cannot be created until it is renamed.
- [`juicefs dump`](../reference/command_reference.mdx#dump) and [`juicefs load`](../reference/command_reference.mdx#load) keep snapshots, in both the JSON and the binary format. A dump records the metadata version the volume has when it is taken, even if the dumping client loaded an older one, and fails if the first snapshot is created while it runs; dump again then. A dump of a volume that has held snapshots therefore always carries metadata version 2, and must be loaded with a client that supports it: a load checks the metadata version of a dump before writing anything, and refuses a dump it is too old for. Clients older than 1.5 do not make that check: they load everything but the snapshots, then fail, leaving an incomplete volume behind.
- A dump taken from inside `/.snapshots`, or of a snapshot itself, is refused: its entries stay frozen wherever they are loaded, so the volume it would produce could never be changed or emptied. [Clone](./clone.md) the snapshot into a live directory and dump that instead.
