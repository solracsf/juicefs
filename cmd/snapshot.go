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
	"text/tabwriter"
	"time"

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
metadata space. Snapshots are read-only and live under /.snapshots.

Examples:
# Snapshot the whole volume
$ juicefs snapshot create redis://localhost --path / --name daily-2026-08-20

# Snapshot one directory
$ juicefs snapshot create redis://localhost --path /data --name before-upgrade

# List them
$ juicefs snapshot list redis://localhost`,
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
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tCREATED\tPATH")
	for _, s := range snaps {
		_, _ = fmt.Fprintf(w, "%s\t%s\t/%s/%s\n", s.Name,
			s.Created.Format(time.RFC3339), meta.SnapshotName, s.Name)
	}
	return w.Flush()
}
