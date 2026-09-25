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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
)

// snapshotChildEnv carries the arguments of a "juicefs snapshot" command to a
// child test process, since an unknown option ends the process.
const snapshotChildEnv = "JFS_TEST_SNAPSHOT_ARGS"

// Each snapshot subcommand takes its own flags only: one of another subcommand
// is refused rather than silently accepted, so that "snapshot delete" cannot
// be run with, say, --best-effort or --tag.
func TestSnapshotSubcommandFlags(t *testing.T) {
	if args := os.Getenv(snapshotChildEnv); args != "" {
		if err := Main(append([]string{"juicefs"}, strings.Fields(args)...)); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	metaURL := "sqlite3://" + filepath.Join(t.TempDir(), "test.db")
	bucket := filepath.Join(t.TempDir(), "testBucket")
	if err := Main([]string{"", "format", metaURL, "--bucket", bucket, "--trash-days", "0", testVolume}); err != nil {
		t.Fatalf("format: %s", err)
	}
	m := meta.NewClient(metaURL, nil)
	if _, err := m.Load(true); err != nil {
		t.Fatalf("load: %s", err)
	}
	defer func() { _ = m.Shutdown() }()
	var d meta.Ino
	if st := m.Mkdir(meta.Background(), meta.RootInode, "d", 0755, 0, 0, &d, nil); st != 0 {
		t.Fatalf("mkdir d: %s", st)
	}
	if err := Main([]string{"", "snapshot", "create", metaURL, "--path", "/d", "--name", "s"}); err != nil {
		t.Fatalf("snapshot create: %s", err)
	}
	holds := func() []string {
		t.Helper()
		snaps, st := m.ListSnapshots(meta.Background())
		if st != 0 || len(snaps) != 1 || snaps[0].Name != "s" {
			t.Fatalf("snapshot s is gone: %v %s", snaps, st)
		}
		return snaps[0].Holds
	}
	run := func(args string) error {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestSnapshotSubcommandFlags$")
		cmd.Env = append(os.Environ(), snapshotChildEnv+"="+args)
		out, err := cmd.CombinedOutput()
		t.Logf("juicefs %s: %v\n%s", args, err, out)
		return err
	}
	for _, args := range []string{
		"snapshot delete " + metaURL + " --name s --best-effort",
		"snapshot delete " + metaURL + " --name s --tag x",
		"snapshot delete " + metaURL + " --name s --path /d",
		"snapshot hold " + metaURL + " --name s --tag x --path /d",
		"snapshot release " + metaURL + " --name s --tag x --best-effort",
		"snapshot list " + metaURL + " --name s",
	} {
		if run(args) == nil {
			t.Fatalf("juicefs %s succeeded", args)
		}
		if h := holds(); len(h) != 0 {
			t.Fatalf("juicefs %s left holds %v", args, h)
		}
	}
	if err := Main([]string{"", "snapshot", "hold", metaURL, "--name", "s", "--tag", "x"}); err != nil {
		t.Fatalf("snapshot hold: %s", err)
	}
	if h := holds(); len(h) != 1 || h[0] != "x" {
		t.Fatalf("holds after hold: %v", h)
	}
	if err := Main([]string{"", "snapshot", "release", metaURL, "--name", "s", "--tag", "x"}); err != nil {
		t.Fatalf("snapshot release: %s", err)
	}
	if err := Main([]string{"", "snapshot", "delete", metaURL, "--name", "s"}); err != nil {
		t.Fatalf("snapshot delete: %s", err)
	}
	if snaps, st := m.ListSnapshots(meta.Background()); st != 0 || len(snaps) != 0 {
		t.Fatalf("snapshots after delete: %v %s", snaps, st)
	}
}
