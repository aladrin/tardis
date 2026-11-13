# Installing Tardis on OpenBSD

Tardis is a single-binary, zero-CGO backup and selective restore utility for OpenBSD FFS filesystems. It streams `/sbin/dump` through pure-Go `zstd`, OpenBSD base **LibreSSL** (`ChaCha20` with `PBKDF2-HMAC-SHA256`, 100,000 iterations), and domain-bound `HMAC-SHA256` directly to Amazon S3 `DEEP_ARCHIVE`, while maintaining fast, authenticated file catalogs in S3 `STANDARD` for interactive browsing and point-in-time recovery.

---

## 1. Requirements

### Target OpenBSD Host (Runtime)
Tardis compiles to a standalone static binary (`CGO_ENABLED=0`) and requires **no third-party packages or ports at runtime**—only standard OpenBSD base utilities:

* **OpenBSD** (7.4 or newer recommended; `amd64`, `arm64`, or `mips64`/`octeon`)
* **OpenBSD Base LibreSSL** (`/usr/bin/openssl`)
* **OpenBSD Base Dump/Restore** (`/sbin/dump`, `/sbin/restore`, `/sbin/mount`, `/etc/dumpdates`)
* **Root privileges** (`doas` or `root` for raw device reads via `/sbin/dump`, metadata restoration via `/sbin/restore`, and `0400` key storage in `/etc/tardis.d`)
* **S3-compatible object storage** (AWS S3 with Glacier Deep Archive, or compatible S3 endpoint)

### Build Host
* **Go 1.22 or newer** (`pkg_add go` on OpenBSD, or any Linux/macOS/BSD host for cross-compilation)
* **Git** (`pkg_add git`)

---

## 2. Building from Source

### Option A: Native Build on OpenBSD

```sh
pkg_add go git
git clone https://github.com/aladrin/tardis.git
cd tardis
sh ./build.sh
```

### Option B: Cross-Compiling for OpenBSD (e.g., from Linux or macOS)

Because `tardis` uses `CGO_ENABLED=0`, pure-Go `zstd`, and native `golang.org/x/sys/unix` ioctls, you can build OpenBSD binaries on any workstation:

```sh
git clone [https://github.com/aladrin/tardis.git](https://github.com/aladrin/tardis.git)
cd tardis

# OpenBSD x86_64
GOOS=openbsd GOARCH=amd64 sh ./build.sh

# OpenBSD ARM64 (e.g., AWS Graviton / Raspberry Pi / Ampere)
GOOS=openbsd GOARCH=arm64 sh ./build.sh

# OpenBSD MIPS64 (e.g., Ubiquiti EdgeRouter Octeon)
GOOS=openbsd GOARCH=mips64 sh ./build.sh
```

Copy the resulting `./tardis` binary to your target OpenBSD host via `scp`.

---

## 3. System Installation & Directory Layout

Install the binary into `/usr/local/sbin` and initialize the restricted configuration and state directories:

```sh
doas install -o root -g wheel -m 0755 tardis /usr/local/sbin/tardis
doas install -d -o root -g wheel -m 0700 /etc/tardis.d /var/db/tardis /var/db/tardis/jobs
```

### Filesystem Paths Used by Tardis

| Path | Permissions | Purpose |
| :--- | :--- | :--- |
| `/usr/local/sbin/tardis` | `0755` | Main CLI and interactive terminal UI binary |
| `/etc/tardis.d/config.json` | `0600` | S3 bucket, region, credentials, client ID, and default tier settings |
| `/etc/tardis.d/<client>.key` | `0400` | 128-hex-char master keyfile (256-bit encryption secret + 256-bit MAC key) |
| `/var/db/tardis/<client>/` | `0700` | Local `zstd` cache of decrypted/verified S3 `STANDARD` snapshot catalogs |
| `/var/db/tardis/jobs/` | `0700` | Persistent JSON state for queued and thawing Glacier restore jobs |
| `/var/run/tardis.lock` | `0600` | Kernel `flock(2)` mutual-exclusion lock during active dump/restore runs |
| `/var/log/tardis` | `0600` | Operational and security audit log |

---

## 4. AWS S3 Bucket & IAM Setup

1. Create a private S3 bucket (with **Block All Public Access** enabled and default bucket encryption active).
2. Create an IAM user or role for your OpenBSD host(s) with the following least-privilege policy (replace `YOUR-BUCKET-NAME` with your bucket):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "TardisBucketList",
      "Effect": "Allow",
      "Action": [
        "s3:ListBucket",
        "s3:GetBucketLocation"
      ],
      "Resource": "arn:aws:s3:::YOUR-BUCKET-NAME"
    },
    {
      "Sid": "TardisObjectOperations",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:RestoreObject",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": "arn:aws:s3:::YOUR-BUCKET-NAME/*"
    }
  ]
}
```

### S3 Object Layout per Snapshot
For each backup, Tardis writes four objects under `s3://<bucket>/<client>/`:
* `<timestamp>_<client><mount>_<level>.zst.dat` (`DEEP_ARCHIVE`) — Compressed, LibreSSL-encrypted BSD `dump(8)` stream
* `<timestamp>_<client><mount>_<level>.hmac` (`STANDARD`) — Domain-bound `HMAC-SHA256` authentication record
* `<timestamp>_<client><mount>_<level>_list.zst.dat` (`STANDARD`) — Encrypted JSONL file/inode catalog for instant browsing
* `<timestamp>_<client><mount>_<level>_list.hmac` (`STANDARD`) — Domain-bound `HMAC-SHA256` record for the catalog

---

## 5. Configuration & Cryptographic Key Initialization

You can configure Tardis either interactively through the terminal UI or directly via `/etc/tardis.d/config.json`.

### Option A: Interactive TUI Setup (Recommended)

Launch Tardis as `root` (or via `doas`):

```sh
doas tardis
```

When `/etc/tardis.d/config.json` does not yet exist, Tardis opens directly on **View 4 (`S3 & SYSTEM CONFIG`)**:
1. Use `▲` / `▼` and `ENTER` to populate **S3 Bucket**, **AWS Region**, **Client Identifier**, and credentials (**AWS Shared Profile** or **Access Key ID** + **Secret Access Key**).
2. Select **`[ Generate / Verify Client 0400 Keyfile ]`** (or press `t`) to atomically generate `/etc/tardis.d/<client>.key` (`0400`) and run a live LibreSSL `ChaCha20+PBKDF2+HMAC-SHA256` + S3 connectivity self-test.
3. Press **`s`** to save `/etc/tardis.d/config.json` with `0600` permissions.

### Option B: Manual Configuration File

Create `/etc/tardis.d/config.json` with `0600` permissions:

```sh
doas install -m 0600 /dev/null /etc/tardis.d/config.json
doas tee /etc/tardis.d/config.json > /dev/null << 'EOF'
{
  "bucket": "your-tardis-backup-bucket",
  "region": "ap-southeast-2",
  "endpoint": "",
  "force_path_style": false,
  "aws_profile": "",
  "access_key_id": "AKIA...",
  "secret_access_key": "YOUR_SECRET_ACCESS_KEY",
  "client": "nettle",
  "key_path": "/etc/tardis.d/nettle.key",
  "default_tier": "Bulk"
}
EOF
```

*(If `aws_profile`, `access_key_id`, and `secret_access_key` are left blank, Tardis automatically falls back to standard AWS SDK environment variables `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, `~/.aws/credentials`, or EC2/ECS IAM instance roles.)*

### Verify Crypto & S3 Connectivity

Run the built-in self-test (`-S`). If the client keyfile does not exist yet, `-S` generates it atomically with `0400` permissions, verifies LibreSSL encryption/decryption and tamper rejection, and tests S3 bucket access:

```sh
doas tardis -S
```

Expected output:

```text
crypto : PASS — LibreSSL 4.0.0 | ChaCha20-PBKDF2(100k) + HMAC-SHA256
keyfile: /etc/tardis.d/nettle.key [generated (0400), id=a1b2c3d4e5f6]
s3     : PASS — s3://your-tardis-backup-bucket/nettle/ reachable (ap-southeast-2)
```

> **CRITICAL — BACK UP YOUR KEYFILE OFFLINE:**  
> Store a copy of `/etc/tardis.d/<client>.key` in an offline password manager or encrypted vault (such as Vaultwarden or KeePassXC). Without this exact 128-character keyfile, backups stored in S3 `DEEP_ARCHIVE` cannot be decrypted or authenticated.

---

## 6. Automated Backups & Restore Polling (`cron`)

OpenBSD `/sbin/dump` only supports `ffs` filesystems. Inspect mounted FFS partitions on your system with:

```sh
mount -t ffs
```

Add scheduled dump jobs and background Glacier thaw polling to `root`'s crontab (`doas crontab -e`):

```cron
# Monthly Level 0 full dumps on the 1st of each month
0  2  1  *  *   /usr/local/sbin/tardis -m / -d 0
15 2  1  *  *   /usr/local/sbin/tardis -m /var -d 0
30 2  1  *  *   /usr/local/sbin/tardis -m /home -d 0

# Weekly Level 1 dumps every Sunday (except the 1st)
0  3  2-31 * 0  /usr/local/sbin/tardis -m / -d 1
15 3  2-31 * 0  /usr/local/sbin/tardis -m /var -d 1
30 3  2-31 * 0  /usr/local/sbin/tardis -m /home -d 1

# Daily Level 2 incremental dumps Monday-Saturday (exits cleanly if no inodes changed)
0  4  *  * 1-6  /usr/local/sbin/tardis -m / -d 2
10 4  *  * 1-6  /usr/local/sbin/tardis -m /var -d 2
20 4  *  * 1-6  /usr/local/sbin/tardis -m /home -d 2

# Poll queued/thawing Glacier Deep Archive restore jobs every 30 minutes
*/30 * * * *    /usr/local/sbin/tardis -J
```

---

## 7. Quick Usage Reference

### Interactive Terminal UI (`doas tardis` or `doas tardis -u`)
* **`◄` / `►` (or `Tab`)**: Switch between **Dump Timeline (1/4)**, **Snapshot Browser (2/4)**, **Jobs & Glacier Monitor (3/4)**, and **S3 & System Config (4/4)**.
* **View 1 (Timeline)**: `1`–`9` filters by mountpoint (`0` resets); `ENTER` opens the snapshot catalog; `T` initiates a Glacier thaw; `D` deletes a snapshot bundle.
* **View 2 (Browser)**: `[` and `]` step backward/forward through dump dates while keeping your cursor anchored on the same path; `SPACE` marks files/folders; `m` toggles **OVERWRITE** vs. **NEW_PATH**; `o` sets alternate destination filename/directory/suffix; `x` restores marked items from the active snapshot (`X` queues the full BSD `L0 -> Ln` incremental chain).

### Headless CLI Operations

```sh
# Query snapshots and live Glacier thaw status
doas tardis -q ALL

# Run an on-demand Level 0 dump of /home
doas tardis -m /home -d 0

# Restore an archive to an alternate directory (auto-thaws DEEP_ARCHIVE and polls until complete)
doas tardis -m /home -r 2026-11-13_20:30:00_nettle_home_0.zst.dat -t Standard -o /home/restore_test
```

---

## 8. Emergency Bare-Metal Recovery (Without `tardis` Binary)

Because Tardis encrypts streams using OpenBSD base LibreSSL (`ChaCha20` with `PBKDF2`, 100,000 iterations) where the encryption passphrase is the exact first 64 ASCII hex characters of `/etc/tardis.d/<client>.key`, any thawed `.zst.dat` archive can be verified and extracted using standard OpenBSD tools alone:

```sh
# 1. Verify HMAC-SHA256 authenticity using the second 64 hex chars (32-byte MAC key)
MAC_KEY=$(tail -c 65 /etc/tardis.d/nettle.key | tr -d '\n')
printf 'TARDIS-V2\0nettle/2026-11-13_20:30:00_nettle_home_0.zst.dat\0' | \
  cat - 2026-11-13_20:30:00_nettle_home_0.zst.dat | \
  openssl dgst -sha256 -mac HMAC -macopt hexkey:"$MAC_KEY"

# 2. Compare output against 2026-11-13_20:30:00_nettle_home_0.hmac, then decrypt and extract
cd /mnt/target
head -c 64 /etc/tardis.d/nettle.key | \
  openssl enc -chacha -pbkdf2 -iter 100000 -d -pass stdin -in /tmp/2026-11-13_20:30:00_nettle_home_0.zst.dat | \
  zstd -d | restore -rf -
```
