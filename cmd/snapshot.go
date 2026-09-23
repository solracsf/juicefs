/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"fmt"
	"os"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/urfave/cli/v2"
)

func cmdSnapshot() *cli.Command {
	return &cli.Command{
		Name:      "snapshot",
		Category:  "ADMIN",
		Usage:     "Manage snapshots of a volume",
		ArgsUsage: "META-URL",
		Description: `
A snapshot is a frozen copy of a directory tree. It copies metadata only, so it
is fast and shares its data with the original, but it does consume inodes and
metadata space. Snapshots are read-only and live under /.snapshots. They are
not counted against the volume capacity or any quota; "list" reports their usage.

Examples:
# Snapshot the whole volume
$ juicefs snapshot create redis://localhost --path / --name daily-2026-08-20

# Snapshot one directory
$ juicefs snapshot create redis://localhost --path /data --name before-upgrade

# List them
$ juicefs snapshot list redis://localhost

# Delete one, releasing the data only it referenced
$ juicefs snapshot delete redis://localhost --name before-upgrade`,
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "path",
				Value: "/",
				Usage: "the directory to snapshot, relative to the volume root",
			},
			&cli.StringFlag{
				Name:  "name",
				Usage: "name of the snapshot",
			},
		},
		Subcommands: []*cli.Command{
			{
				Name:      "create",
				Usage:     "Create a snapshot of a directory tree",
				ArgsUsage: "META-URL",
				Action:    snapshotCreate,
			},
			{
				Name:      "list",
				Usage:     "List the snapshots of a volume",
				ArgsUsage: "META-URL",
				Action:    snapshotList,
			},
			{
				Name:      "delete",
				Usage:     "Delete a snapshot and release the data only it referenced",
				ArgsUsage: "META-URL",
				Action:    snapshotDelete,
			},
		},
	}
}

func snapshotCreate(c *cli.Context) error {
	setup(c, 1)
	removePassword(c.Args().Get(0))
	m := meta.NewClient(c.Args().Get(0), nil)
	if _, err := m.Load(true); err != nil {
		return err
	}
	defer func() { _ = m.Shutdown() }()

	name := c.String("name")
	if name == "" {
		return fmt.Errorf("please give the snapshot a name with --name")
	}
	ctx := meta.Background()
	src := meta.RootInode
	if p := c.String("path"); p != "" && p != "/" {
		var attr meta.Attr
		if st := m.Resolve(ctx, meta.RootInode, p, &src, &attr, true); st != 0 {
			return fmt.Errorf("resolve %s: %s", p, st)
		}
	}

	progress := utils.NewProgress(false)
	bar := progress.AddCountBar("Copied entries", 0)
	var count, total uint64
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond * 200):
				bar.SetTotal(int64(total))
				bar.SetCurrent(int64(count))
			}
		}
	}()
	root, st := m.CreateSnapshot(ctx, src, name, &count, &total)
	close(done)
	bar.SetTotal(int64(total))
	bar.SetCurrent(int64(count))
	bar.Done()
	progress.Done()
	if st != 0 {
		return fmt.Errorf("create snapshot %s: %s", name, st)
	}
	logger.Infof("snapshot %s created at /%s/%s (inode %d, %d entries)",
		name, meta.SnapshotName, name, root, count)
	return nil
}

func snapshotList(c *cli.Context) error {
	setup(c, 1)
	removePassword(c.Args().Get(0))
	m := meta.NewClient(c.Args().Get(0), nil)
	if _, err := m.Load(true); err != nil {
		return err
	}
	defer func() { _ = m.Shutdown() }()

	snaps, st := m.ListSnapshots(meta.Background())
	if st != 0 {
		return fmt.Errorf("list snapshots: %s", st)
	}
	if len(snaps) == 0 {
		logger.Infof("no snapshot")
		return nil
	}
	// snapshots are not charged against capacity or quotas, so this is where their
	// usage shows up; they are immutable, so the figures cannot go stale
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tCREATED\tINODES\tSIZE\tPATH")
	var inodes, size uint64
	for _, s := range snaps {
		var sum meta.Summary
		if st := m.GetSummary(meta.Background(), s.Inode, &sum, true, false); st != 0 {
			return fmt.Errorf("summary of snapshot %s: %s", s.Name, st)
		}
		inodes += sum.Dirs + sum.Files
		size += sum.Size
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t/%s/%s\n", s.Name, s.Created.Format(time.RFC3339),
			sum.Dirs+sum.Files, humanize.IBytes(sum.Size), meta.SnapshotName, s.Name)
	}
	_, _ = fmt.Fprintf(w, "(%d snapshots)\t\t%d\t%s\t\n", len(snaps), inodes, humanize.IBytes(size))
	return w.Flush()
}

func snapshotDelete(c *cli.Context) error {
	setup(c, 1)
	removePassword(c.Args().Get(0))
	metaConf := meta.DefaultConf()
	metaConf.NoBGJob = true
	m := meta.NewClient(c.Args().Get(0), metaConf)
	format, err := m.Load(true)
	if err != nil {
		return err
	}
	defer func() { _ = m.Shutdown() }()

	name := c.String("name")
	if name == "" {
		return fmt.Errorf("please give the snapshot to delete with --name")
	}
	// the blocks only this snapshot referenced are removed as it goes
	blob, err := createStorage(*format)
	if err != nil {
		return fmt.Errorf("object storage: %s", err)
	}
	chunkConf := *getDefaultChunkConf(format)
	chunkConf.CacheDir = "memory"
	store := chunk.NewCachedStore(blob, chunkConf, nil)
	m.OnMsg(meta.DeleteSlice, func(args ...interface{}) error {
		return store.Remove(args[0].(uint64), int(args[1].(uint32)))
	})

	progress := utils.NewProgress(false)
	spin := progress.AddCountSpinner("Removed entries")
	var count uint64
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond * 200):
				spin.SetCurrent(int64(atomic.LoadUint64(&count)))
			}
		}
	}()
	st := m.DeleteSnapshot(meta.Background(), name, &count)
	close(done)
	spin.SetCurrent(int64(count))
	spin.Done()
	progress.Done()
	if st != 0 {
		return fmt.Errorf("delete snapshot %s: %s", name, st)
	}
	logger.Infof("snapshot %s deleted (%d entries)", name, count)
	return nil
}
