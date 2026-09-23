#!/usr/bin/env python3
"""Workload and oracle for the snapshot battle test (snapshot.sh).

Writers mutate a tree through a mount while snapshots are taken. Every block a
writer produces describes itself (file, block index, generation) and carries a
body derived from that header, so a block read back can be checked on its own.
Each writer journals when a generation starts and when it is committed (the
file is closed), which bounds what a snapshot taken in [start, end] may hold:

  - a state file or big-file block holds a generation that was committed
    before the snapshot started, or a later one that started before it ended;
  - a marker committed before the snapshot started is present, and one that
    started after it ended is absent;
  - every file is readable and every block is intact, whatever churn ran.

A best-effort snapshot only has to meet those bounds file by file. A consistent
one (--atomic) must also be the tree at one instant: some T within the create
call after every generation and marker it holds was begun, and before the next
generation, or a marker it lacks, was committed.

Writers pause while a file named "pause" exists next to their journal.

Subcommands:
  setup    DIR WRITERS                  create the shared tree
  write    DIR WRITER JOURNAL SECONDS   mutate DIR as writer WRITER
  verify   SNAP JOURNAL... --start NS --end NS [--prev GENS] [--gens OUT] [--atomic]
  manifest DIR OUT                      record a tree: stat, xattrs, content hash
  compare  A B [--ignore FIELD]         fail unless two manifests are identical
  read     DIR SECONDS                  keep reading DIR, fail on a torn block
  aba-setup DIR / aba-run DIR ROLE SECONDS / aba-check SNAP
                                        changes undone over and over, and a check
                                        that a snapshot holds no state that never existed
"""
import argparse
import errno
import hashlib
import json
import os
import random
import signal
import stat
import sys
import time

BLOCK = 4096
HEAD = 64
STATE_FILES = 16
BIG_BLOCKS = 256  # 1 MiB, rewritten block by block so it fragments into slices


def block(name, idx, gen):
    head = f"{name}:{idx}:{gen}:".encode().ljust(HEAD, b"#")
    body, seed = b"", hashlib.sha256(head).digest()
    while len(body) < BLOCK - HEAD:
        seed = hashlib.sha256(seed).digest()
        body += seed
    return head + body[:BLOCK - HEAD]


def parse(buf):
    """Return (name, idx, gen) of an intact block, or None."""
    if len(buf) != BLOCK:
        return None
    try:
        name, idx, gen, _ = buf[:HEAD].split(b":", 3)
        name, idx, gen = name.decode(), int(idx), int(gen)
    except ValueError:
        return None
    return (name, idx, gen) if buf == block(name, idx, gen) else None


def state_name(w, i):
    return f"s{w}_{i}"


def big_name(w):
    return f"big{w}"


def setup(root, writers):
    for w in range(writers):
        d = os.path.join(root, f"w{w}")
        for sub in ("state", "markers", "churn"):
            os.makedirs(os.path.join(d, sub), exist_ok=True)
        for i in range(STATE_FILES):
            with open(os.path.join(d, "state", state_name(w, i)), "wb") as f:
                f.write(block(state_name(w, i), 0, 0))
        with open(os.path.join(d, big_name(w)), "wb") as f:
            for i in range(BIG_BLOCKS):
                f.write(block(big_name(w), i, 0))


def write(root, w, journal, seconds):
    stop = []
    signal.signal(signal.SIGTERM, lambda *_: stop.append(1))
    rnd = random.Random(int(os.environ.get("SEED", time.time_ns())) + w)
    d = os.path.join(root, f"w{w}")
    # a writer restarted after its client died carries on the numbering
    gens, marker = {}, 0
    if os.path.exists(journal):
        for (name, idx), evs in load_journals([journal]).items():
            top = max(g for _, _, g in evs)
            if name.startswith("m"):
                marker = max(marker, top + 1)
            else:
                gens[(name, idx)] = top
    deadline = time.time() + seconds
    log = open(journal, "a", buffering=1)

    def rec(event, name, idx, gen):
        log.write(f"{time.time_ns()} {event} {name} {idx} {gen}\n")

    def overwrite(path, name, idx):
        gen = gens.get((name, idx), 0) + 1
        gens[(name, idx)] = gen
        rec("start", name, idx, gen)
        fd = os.open(path, os.O_WRONLY)
        try:
            os.pwrite(fd, block(name, idx, gen), idx * BLOCK)
        finally:
            os.close(fd)
        rec("done", name, idx, gen)

    def churn():
        c = os.path.join(d, "churn")
        names = os.listdir(c)
        op = rnd.choice(["create", "create", "unlink", "rename", "mkdir", "rmdir",
                         "symlink", "link", "chmod", "xattr", "truncate"])
        target = os.path.join(c, rnd.choice(names)) if names else None
        fresh = os.path.join(c, f"c_{rnd.randrange(1 << 30)}")
        if op == "create" or target is None:
            with open(fresh, "wb") as f:
                f.write(block(os.path.basename(fresh), 0, 0))
        elif op == "unlink" and os.path.isfile(target):
            os.unlink(target)
        elif op == "rename":
            os.rename(target, os.path.join(c, f"c_{rnd.randrange(1 << 30)}"))
        elif op == "mkdir":
            os.mkdir(os.path.join(c, f"d_{rnd.randrange(1 << 30)}"))
        elif op == "rmdir" and os.path.isdir(target) and not os.path.islink(target) and not os.listdir(target):
            os.rmdir(target)
        elif op == "symlink":
            os.symlink(os.path.basename(target), os.path.join(c, f"l_{rnd.randrange(1 << 30)}"))
        elif op == "link" and os.path.isfile(target) and not os.path.islink(target):
            os.link(target, os.path.join(c, f"c_{rnd.randrange(1 << 30)}"))
        elif op == "chmod" and not os.path.islink(target):
            os.chmod(target, rnd.choice([0o600, 0o644, 0o755]))
        elif op == "xattr" and not os.path.islink(target):
            os.setxattr(target, "user.gen", str(rnd.randrange(1000)).encode())
        elif op == "truncate" and os.path.isfile(target) and not os.path.islink(target):
            os.truncate(target, 0)

    pause = os.path.join(os.path.dirname(journal), "pause")
    while not stop and time.time() < deadline:
        if os.path.exists(pause):
            time.sleep(0.05)
            continue
        time.sleep(rnd.random() * 0.02)
        r = rnd.random()
        try:
            if r < 0.45:
                i = rnd.randrange(STATE_FILES)
                overwrite(os.path.join(d, "state", state_name(w, i)), state_name(w, i), 0)
            elif r < 0.65:
                overwrite(os.path.join(d, big_name(w)), big_name(w), rnd.randrange(BIG_BLOCKS))
            elif r < 0.75:
                name = f"m{w}_{marker}"
                rec("start", name, 0, marker)
                with open(os.path.join(d, "markers", name), "wb") as f:
                    f.write(block(name, 0, marker))
                rec("done", name, 0, marker)
                marker += 1
            else:
                churn()
        except OSError as e:
            # churn races with itself and trips over what it made (a symlink to a
            # directory, a name taken meanwhile); only a failing mount stops the writer
            if e.errno not in (errno.EIO, errno.ENOTCONN, errno.ECONNABORTED):
                continue
            print(f"writer {w}: {e}", file=sys.stderr)
            sys.exit(2)


def load_journals(paths):
    """Per (name, idx): list of (start_ns, done_ns or None, gen)."""
    events = {}
    for p in paths:
        with open(p) as f:
            for line in f:
                parts = line.split()
                if len(parts) != 5:
                    continue  # a line cut short by a kill
                ns, ev, name, idx, gen = int(parts[0]), parts[1], parts[2], int(parts[3]), int(parts[4])
                if ev == "start":
                    events.setdefault((name, idx), []).append([ns, None, gen])
                else:
                    for e in events.get((name, idx), []):
                        if e[2] == gen:
                            e[1] = ns
    return events


def bounds(events, key, start, end, initial):
    """Lowest and highest generation a snapshot over [start, end] may hold."""
    lo = hi = initial
    for s, d, g in events.get(key, []):
        if d is not None and d < start:
            lo = max(lo, g)
        if s < end:
            hi = max(hi, g)
    return lo, hi


def instant(events, gens, markers, start, end):
    """The window in which one instant T explains everything the snapshot holds:
    each generation g it holds was committed by T (so T >= start of g) and its
    successor was not (so T < end of g+1); each marker it holds was committed by
    T, and each one it lacks was not. Returns (lo, why lo, hi, why hi)."""
    lo, hi, why_lo, why_hi = start, end, "the create call began", "the create call ended"
    for key, g in gens.items():
        for s, d, gen in events.get(key, []):
            if gen == g and s > lo:
                lo, why_lo = s, f"{key} holds generation {g}, begun then"
            elif gen == g + 1 and d is not None and d < hi:
                hi, why_hi = d, f"{key} lacks generation {g + 1}, committed then"
    for (name, _), evs in events.items():
        if name[0] != "m":
            continue
        for s, d, _ in evs:
            if name in markers and s > lo:
                lo, why_lo = s, f"marker {name} is present, begun then"
            elif name not in markers and d is not None and d < hi:
                hi, why_hi = d, f"marker {name} is absent, committed then"
    return lo, why_lo, hi, why_hi


def verify(snap, journals, start, end, prev, gens_out, atomic):
    events = load_journals(journals)
    errors, gens = [], {}
    held, markers = {}, set()

    def err(msg):
        errors.append(msg)

    # the whole tree must be readable, whatever the churn left in it
    for dirpath, dirs, files in os.walk(snap):
        for n in files:
            p = os.path.join(dirpath, n)
            try:
                if os.path.islink(p):
                    os.readlink(p)
                    continue
                with open(p, "rb") as f:
                    data = f.read()
            except OSError as e:
                err(f"{p}: unreadable: {e}")
                continue
            if n.startswith("c_") and data:
                for off in range(0, len(data), BLOCK):
                    if parse(data[off:off + BLOCK]) is None:
                        err(f"{p}: torn block at {off}")

    for wdir in sorted(os.listdir(snap)):
        if not wdir.startswith("w"):
            continue
        w = int(wdir[1:])
        base = os.path.join(snap, wdir)
        for i in range(STATE_FILES):
            name = state_name(w, i)
            try:
                with open(os.path.join(base, "state", name), "rb") as f:
                    got = parse(f.read())
            except OSError as e:
                err(f"{name}: missing: {e}")
                continue
            if got is None or got[0] != name:
                err(f"{name}: not an intact block")
                continue
            lo, hi = bounds(events, (name, 0), start, end, 0)
            if not lo <= got[2] <= hi:
                err(f"{name}: generation {got[2]} outside [{lo}, {hi}]")
            gens[name] = got[2]
            held[(name, 0)] = got[2]
        name = big_name(w)
        try:
            with open(os.path.join(base, name), "rb") as f:
                data = f.read()
        except OSError as e:
            err(f"{name}: unreadable: {e}")
            data = b""
        if len(data) != BIG_BLOCKS * BLOCK:
            err(f"{name}: size {len(data)}, want {BIG_BLOCKS * BLOCK}")
        for i in range(len(data) // BLOCK):
            got = parse(data[i * BLOCK:(i + 1) * BLOCK])
            if got is None or got[:2] != (name, i):
                err(f"{name}: block {i} is not intact")
                continue
            lo, hi = bounds(events, (name, i), start, end, 0)
            if not lo <= got[2] <= hi:
                err(f"{name}: block {i} generation {got[2]} outside [{lo}, {hi}]")
            gens[f"{name}:{i}"] = got[2]
            held[(name, i)] = got[2]
        # markers: every one committed before the start is in, none begun after the end
        present = set(os.listdir(os.path.join(base, "markers")))
        markers |= present
        for (mname, _), evs in events.items():
            if not mname.startswith(f"m{w}_"):
                continue
            for s, d, g in evs:
                if d is not None and d < start and mname not in present:
                    err(f"{mname}: committed before the snapshot but missing")
                if s > end and mname in present:
                    err(f"{mname}: begun after the snapshot but present")
        for mname in present:
            with open(os.path.join(base, "markers", mname), "rb") as f:
                data = f.read()
            if parse(data) is not None:
                continue
            # a marker is created, then written: a client stopped in between
            # has committed only part of it, which a snapshot taken before that
            # write completed may hold
            if not any((d is None or d >= start) and block(mname, 0, g).startswith(data)
                       for _, d, g in events.get((mname, 0), [])):
                err(f"{mname}: not an intact block")

    # a consistent snapshot is the tree at one instant within the create call
    if atomic:
        lo, why_lo, hi, why_hi = instant(events, held, markers, start, end)
        if lo > hi:
            err(f"no single instant explains the snapshot: {why_lo} ({lo}), but {why_hi} ({hi})")

    # snapshots taken one after the other never go back in time
    if prev:
        with open(prev) as f:
            before = json.load(f)
        for k, g in gens.items():
            if k in before and g < before[k]:
                err(f"{k}: generation {g} is older than {before[k]} in the previous snapshot")
    if gens_out:
        with open(gens_out, "w") as f:
            json.dump(gens, f)
    for e in errors[:50]:
        print("VIOLATION:", e)
    print(f"verified {snap}: {len(gens)} generations checked, {len(errors)} violations")
    return 1 if errors else 0


# ---- states that never exist -------------------------------------------------
# Changes that are undone again and again, so a check that only compares values
# before and after would be fooled (A -> B -> A). Each keeps an invariant that no
# real state of the tree breaks, which a snapshot at one instant cannot break:
#   A/x, B/x and C/x: a file moved round A -> B -> C -> A, so exactly one exists;
#   p/P and q/Q: counters mod 3, P written first, so (P - Q) % 3 is 0 or 1;
#   xp/F and xq/F: the same counters in extended attributes.
ABA_FILLER = 300


def aba_setup(root):
    for d in ("A", "B", "C", "p", "q", "xp", "xq"):
        os.makedirs(os.path.join(root, d), exist_ok=True)
        for i in range(ABA_FILLER):
            with open(os.path.join(root, d, f"fill{i}"), "wb") as f:
                f.write(b"x")
    open(os.path.join(root, "A", "x"), "wb").close()
    for d, n in (("p", "P"), ("q", "Q")):
        with open(os.path.join(root, d, n), "wb") as f:
            f.write(b"0")
    for d in ("xp", "xq"):
        path = os.path.join(root, d, "F")
        open(path, "wb").close()
        os.setxattr(path, "user.v", b"0")


def aba_run(root, role, seconds):
    rnd = random.Random(int(os.environ.get("SEED", time.time_ns())))
    deadline, k = time.time() + seconds, 0
    # bursts with short quiet gaps: changes can then stop halfway through a copy,
    # leaving the comparison to face a tree that looks just like it did before
    burst_end = time.time() + rnd.uniform(0.5, 2)
    while time.time() < deadline:
        if time.time() > burst_end:
            time.sleep(rnd.uniform(0.5, 1.5))
            burst_end = time.time() + rnd.uniform(0.5, 2)
        k += 1
        v = str(k % 3).encode()
        if role == "rename":
            for src, dst in (("A", "B"), ("B", "C"), ("C", "A")):
                os.rename(os.path.join(root, src, "x"), os.path.join(root, dst, "x"))
                time.sleep(rnd.random() * 0.005)
        elif role == "pairs":
            for d, n in (("p", "P"), ("q", "Q")):
                fd = os.open(os.path.join(root, d, n), os.O_WRONLY)
                os.pwrite(fd, v, 0)
                os.close(fd)
        else:
            for d in ("xp", "xq"):
                os.setxattr(os.path.join(root, d, "F"), "user.v", v)
        time.sleep(rnd.random() * 0.01)
    print(f"{role}: {k} rounds")


def aba_check(snap):
    bad = []
    at = {d: int(os.path.exists(os.path.join(snap, d, "x"))) for d in ("A", "B", "C")}
    if sum(at.values()) != 1:
        bad.append("forbidden namespace state: " + " ".join(f"{d}={n}" for d, n in at.items())
                   + " (x is always in exactly one of A, B and C)")
    with open(os.path.join(snap, "p", "P"), "rb") as f:
        p = int(f.read())
    with open(os.path.join(snap, "q", "Q"), "rb") as f:
        q = int(f.read())
    if (p - q) % 3 not in (0, 1):
        bad.append(f"forbidden contents: P={p} Q={q} (Q never runs ahead of P)")
    xp = int(os.getxattr(os.path.join(snap, "xp", "F"), "user.v"))
    xq = int(os.getxattr(os.path.join(snap, "xq", "F"), "user.v"))
    if (xp - xq) % 3 not in (0, 1):
        bad.append(f"forbidden extended attributes: P={xp} Q={xq} (Q never runs ahead of P)")
    for b in bad:
        print("NEVER EXISTED:", b)
    print(f"checked {snap}: {len(bad)} states that never existed")
    return 1 if bad else 0


def manifest(root, out):
    entries = {}
    for dirpath, dirs, files in os.walk(root):
        for n in sorted(dirs + files):
            p = os.path.join(dirpath, n)
            st = os.lstat(p)  # before any read, so a read that moves atime shows
            e = {"mode": st.st_mode, "uid": st.st_uid, "gid": st.st_gid, "nlink": st.st_nlink,
                 "size": st.st_size, "mtime": st.st_mtime_ns, "ctime": st.st_ctime_ns,
                 "atime": st.st_atime_ns}
            try:
                e["xattr"] = {k: os.getxattr(p, k, follow_symlinks=False).hex()
                              for k in sorted(os.listxattr(p, follow_symlinks=False))}
            except OSError:
                e["xattr"] = None
            if stat.S_ISLNK(st.st_mode):
                e["target"] = os.readlink(p)
            elif stat.S_ISREG(st.st_mode):
                with open(p, "rb") as f:
                    e["sha256"] = hashlib.sha256(f.read()).hexdigest()
            entries[os.path.relpath(p, root)] = e
    with open(out, "w") as f:
        json.dump(entries, f, sort_keys=True)
    print(f"manifest of {root}: {len(entries)} entries")


def read(root, seconds):
    """Read every file under root over and over. Files may vanish (a snapshot
    being deleted), but whatever is read must be intact."""
    deadline, reads, failed, torn = time.time() + seconds, 0, 0, 0
    while time.time() < deadline:
        for dirpath, dirs, files in os.walk(root):
            for n in files:
                p = os.path.join(dirpath, n)
                try:
                    with open(p, "rb") as f:
                        data = f.read()
                except OSError:
                    failed += 1
                    continue
                reads += 1
                if n[0] in "smb" or n.startswith("c_"):
                    for off in range(0, len(data), BLOCK):
                        if parse(data[off:off + BLOCK]) is None:
                            torn += 1
                            print(f"TORN: {p} at {off}")
    print(f"read {reads} files, {failed} failed reads, {torn} torn blocks")
    return 1 if torn else 0


def compare(a, b, ignore):
    with open(a) as f:
        ma = json.load(f)
    with open(b) as f:
        mb = json.load(f)
    for m in (ma, mb):
        for e in m.values():
            for k in ignore:
                e.pop(k, None)
    diffs = [k for k in sorted(set(ma) | set(mb)) if ma.get(k) != mb.get(k)]
    for k in diffs[:50]:
        print(f"CHANGED: {k}\n  before {ma.get(k)}\n  after  {mb.get(k)}")
    print(f"compared {a} and {b}: {len(diffs)} differences")
    return 1 if diffs else 0


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("setup")
    p.add_argument("dir")
    p.add_argument("writers", type=int)
    p = sub.add_parser("write")
    p.add_argument("dir")
    p.add_argument("writer", type=int)
    p.add_argument("journal")
    p.add_argument("seconds", type=float)
    p = sub.add_parser("verify")
    p.add_argument("snap")
    p.add_argument("journals", nargs="+")
    p.add_argument("--start", type=int, required=True)
    p.add_argument("--end", type=int, required=True)
    p.add_argument("--prev")
    p.add_argument("--gens")
    p.add_argument("--atomic", action="store_true")
    p = sub.add_parser("manifest")
    p.add_argument("dir")
    p.add_argument("out")
    p = sub.add_parser("compare")
    p.add_argument("a")
    p.add_argument("b")
    p.add_argument("--ignore", action="append", default=[], help="a field to leave out")
    p = sub.add_parser("read")
    p.add_argument("dir")
    p.add_argument("seconds", type=float)
    p = sub.add_parser("aba-setup")
    p.add_argument("dir")
    p = sub.add_parser("aba-run")
    p.add_argument("dir")
    p.add_argument("role", choices=["rename", "pairs", "xattrs"])
    p.add_argument("seconds", type=float)
    p = sub.add_parser("aba-check")
    p.add_argument("snap")
    a = ap.parse_args()
    if a.cmd == "setup":
        setup(a.dir, a.writers)
    elif a.cmd == "write":
        write(a.dir, a.writer, a.journal, a.seconds)
    elif a.cmd == "verify":
        sys.exit(verify(a.snap, a.journals, a.start, a.end, a.prev, a.gens, a.atomic))
    elif a.cmd == "manifest":
        manifest(a.dir, a.out)
    elif a.cmd == "read":
        sys.exit(read(a.dir, a.seconds))
    elif a.cmd == "aba-setup":
        aba_setup(a.dir)
    elif a.cmd == "aba-run":
        aba_run(a.dir, a.role, a.seconds)
    elif a.cmd == "aba-check":
        sys.exit(aba_check(a.snap))
    else:
        sys.exit(compare(a.a, a.b, a.ignore))


if __name__ == "__main__":
    main()
