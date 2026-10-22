# macOS qualification (roadmap G4)

The release gate for macOS mounts: every row of roadmap G4 in writeitai/fsit
(`design/roadmap.md`, and `design/designs/macos-mount.md` §7), on one Apple
Silicon Mac on macOS 26, in both session modes (`rw` and `ro`) wherever the
row applies to the mode. `g4.py` runs the steps; results go to
`results/<date>.md`, one section per run, each with its environment.

## Environment recorded per run

- `sw_vers`, `sysctl kern.osversion`, `uname -m`;
- the CLI's `mountable version` and its git revision;
- the mount options in effect: the mount table entry and `nfsstat -m`;
- `/etc/nfs.conf`;
- where the row ran: production (`mountable.io`, filesystem
  `macos-qualification` in the writeitai organisation) or the local dev
  stack.

Production rules: the owner's CLI login; never restart or stop production
services; filesystem limits lowered through the filesystem-limits API are
restored afterwards; a 1-day API key limited to the filesystem wherever a key
is needed, revoked afterwards; every session the runner opens is revoked.

## Rows

Each step lists what is done and what passes. "Both" means it runs in `rw`
and in `ro`.

### Lifecycle (both)

| Step | Pass |
|---|---|
| L1 mount as the user, no `sudo` | the `mounted` event arrives, with the session's mode, within the two-minute start budget |
| L2 options in effect | mount table: `nfs`, `nodev`, `nosuid`, read-only exactly for `ro`, not `async`; `nfsstat -m`: version 3, `ticotsord`, `hard`, rsize/wsize 1048576 |
| L3 `mountable unmount DIR` | exit 0 with the `unmounted` event; no leftovers (L8) |
| L4 SIGINT | as L3 |
| L5 external `umount DIR` | the CLI notices, exits; no leftovers |
| L6 busy (a process with its working directory inside) | `mountable unmount DIR` and SIGINT each find the clean unmount refused, force the mount off at once and exit 1 with the warning that uncommitted writes may be lost, as on Linux (the 30 s budget bounds a clean stop that hangs); no leftovers |
| L7 partial attach (the post-mount check fails) | the mount is force-unmounted and the CLI exits 1; no leftovers (needs the qualification hook, see "Hooks") |
| L8 no leftovers | no mount entry, no CLI process, the socket directory and the mount's cache directory gone, nothing new under `/tmp/mountable-<uid>` (a session stays active until it expires, as on Linux; the runner revokes it) |

### Isolation (both; needs a second local macOS user)

| Step | Pass |
|---|---|
| I1 the second user connects to `nfs.sock` | refused (permission denied on the directory) |
| I2 with warm caches (the owner read the tree and files just before), the second user runs `ls`, `stat`, `cat`, `cd`, `open` for write inside the mount | every one fails with EACCES; only `stat` of the mountpoint itself may answer, from cache |
| I3 I1–I2 again with `/etc/nfs.conf` setting `noopaque_auth`, `async`, `nfc`, `resvport`, `soft` | same results; L2's options unchanged (`hard`, not `async`) |

### Kill switch (both)

With warm data (a file read), the ACCESS cache warm, an open descriptor
reading it, a process whose working directory is inside, and a `mmap` of a
file. Measured with the API up (production) and down (dev stack).

| Step | Pass |
|---|---|
| K1 `mountable sessions revoke` | the mount leaves the mount table and the CLI exits 1 with the revocation reason, within the gateway's 15 s admission bound plus the measured local stop; afterwards no read through the descriptor, the working directory or new opens returns data; what the `mmap` does is recorded |
| K2 expiry (a ticket created with a short `--max-duration`, mounted with `--ticket-stdin`) | as K1, at the certificate's expiry |
| K3 K1 with the API down (dev stack) | the gateway's refusal alone ends the mount once the API confirms it; the measured times recorded |

### Writes (`rw`)

| Step | Pass |
|---|---|
| W1 write+close, write+fsync, `msync(MS_SYNC)` of a `MAP_SHARED` write, `O_SYNC` write: each acknowledged, then the CLI SIGKILLed at once | after a fresh mount the acknowledged data is all there |
| W2 SIGKILL before and after the chunk upload, the entry commit and the reply (hooks), and at random points of a write+fsync loop | every write whose fsync or close returned success is present; a file is never a mix that no acknowledged state had; nothing unacknowledged was reported durable |

### Outages (both)

| Step | Pass |
|---|---|
| O1 renewal: a mount kept past several 2-minute certificate renewals, reading and writing throughout | no error, no interruption |
| O2 API restart, gateway restart, cloud stalls (dev stack) | operations stall and resume, no error reaches applications for stalls within the deadlines; the mount is not lost |
| O3 sleep and wake (the owner puts the Mac to sleep for several minutes) | after wake, I/O resumes; a session that expired during sleep ends the mount as K2 |

### Behaviour (`rw`; `ro` runs the read-only parts)

| Step | Pass |
|---|---|
| B1 the single-mount POSIX suite: pjdfstest (as root, by the owner), plus the runner's checks: create, rename (also over an open file), unlink while open, hard links, symlinks, chmod, truncate, utimes, directories, `mmap` `MAP_SHARED` write + `msync` durability, a read-only mapping refused write access, truncate under a mapping (SIGBUS), a file created `0444` through a writable descriptor, writes after `fchmod(fd, 0)`, SQLite in journal and WAL mode | pjdfstest clean apart from the published exceptions; every runner check as POSIX says |
| B2 editors' atomic saves: `vim`, `sed -i`, `perl -i`, a write-to-temp + rename + directory fsync, TextEdit (the owner, by hand) | the file holds the new content, keeps its mode, no stray temporaries |
| B3 `git`: clone, commit, branch, checkout, `gc`, `fsck --full`, `status` | all succeed, `fsck` clean |
| B4 Finder copies and `._` files: `ditto` with extended attributes, `xattr -w`, `cp` of a file with a resource fork, a Finder copy (the owner, by hand) | content and AppleDouble `._` files arrive; the `._` files are ordinary files in a Files API listing |

### Cross-mount (`rw` and `ro`; needs Docker for a Linux mount)

| Step | Pass |
|---|---|
| X1 a change on the Mac seen on a Linux mount, and one on Linux seen on the Mac: create, write, rename, delete | each visible within the one-second attribute cache plus the subscription's delay, measured |

### Limits (both)

On `macos-qualification`, with its limits lowered and restored afterwards.

| Step | Pass |
|---|---|
| Li1 reads under a spending cap at the filesystem's reservation | an uncached read is refused (errno recorded), nothing hangs |
| Li2 reads past the download quota (dev stack: the organisation download ceiling has no API, and writeitai has a card) | as Li1 |
| Li3 writes at 32 KiB and 1 MiB requests against a byte quota just above current usage | refused with EDQUOT once the quota is reached; data acknowledged before stays |
| Li4 entry quota just above the current count | creates refused with EDQUOT |
| Li5 `mutations_per_minute` low | creates slow down (JUKEBOX, retried by the kernel), none fails |
| Li6 writes under a spending cap at the reservation | refused (errno recorded) |
| Li7 gateway calls and chunks per GiB written at 32 KiB and at 1 MiB requests | recorded (hooks) |

### Dead mounts (both)

| Step | Pass |
|---|---|
| D1 SIGKILL the CLI with a dirty file, a cached file and an open descriptor | within `deadtimeout` (30 s) macOS treats the mount as dead; processes get errors rather than hanging forever; `mountable unmount DIR` removes it; the next `mountable mount` at the same path works |

## Hooks

Kill points (W2), the partial attach (L7) and the call counts (Li7) cannot
be produced from outside the process. They need a qualification build of the
CLI (`-tags qualhooks`, never in a release) whose gRPC and HTTP clients count
calls by method and can SIGKILL the process before or after the n-th call of
a method, and can fail the post-mount check. That is a separate PR.

## Running it

```sh
go build -o /tmp/mountable ./cmd/mountable
python3 qualification/macos-g4/g4.py --cli /tmp/mountable env lifecycle killswitch writes renewal behaviour limits dead
```

Results are appended to `results/<date>.md`. Each group mounts at
`~/mountable-g4-rw` and `~/mountable-g4-ro` (isolation at
`/tmp/g4-isolation-*`, which another user can reach) and keeps its test
data under `g4-*` directories of the filesystem.

### Steps for the owner

These need a person at the Mac, or `sudo`; each runs in one go:

1. **Isolation** (I1–I3): create a standard local user (System Settings >
   Users & Groups), say `mqual`, then in Terminal:
   `python3 qualification/macos-g4/g4.py --cli /tmp/mountable --second-user mqual isolation`.
   It asks for your password once for `sudo -u mqual` and for editing
   `/etc/nfs.conf`, which it restores afterwards.
2. **Sleep and wake** (O3): `python3 qualification/macos-g4/g4.py --cli /tmp/mountable sleep`,
   then, when it says so, Apple menu > Sleep for at least 5 minutes, and
   wake the Mac. It finishes 2 minutes after the wake.
3. **pjdfstest** (B1): with `brew install autoconf automake`,
   `python3 qualification/macos-g4/g4.py --cli /tmp/mountable pjdfstest`
   (it runs the suite with `sudo`).
4. **By hand** (B2, B4), with a mount running
   (`mountable mount FS_ID ~/mnt`): open a text file on the mount in
   TextEdit, change it and save; copy a folder onto the mount in Finder.
5. **Docker Desktop** running, for the dev-stack rows (K3, O2, Li2, Li4, X1).
