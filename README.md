# Installing Tardis (OpenBSD & Linux)

Tardis is a single-binary, zero-CGO backup and selective restore utility for **OpenBSD** and **Linux**. It streams native filesystem dumps through pure-Go `zstd`, `ChaCha20` (`PBKDF2-HMAC-SHA256`, 100,000 iterations), and domain-bound `HMAC-SHA256` directly to Amazon S3 `DEEP_ARCHIVE`, while maintaining fast, authenticated file catalogs in S3 `STANDARD` for interactive terminal browsing and point-in-time recovery.

---

## 1. Platform Support & Runtime Requirements

### OpenBSD
Tardis requires **zero third-party packages or ports at runtime** on OpenBSD—only standard base utilities:
* **Supported Filesystems:** `ffs` (FFS1 / FFS2) via base `/sbin/dump` and `/sbin/restore`
* **SSL / Crypto Engine:** OpenBSD base **LibreSSL** (`/usr/bin/openssl`, `-chacha`)
* **Dump State:** `/etc/dumpdates`

### Linux
Tardis automatically detects filesystem types on `/proc/self/mountinfo` and selects the matching dump/restore driver and SSL provider:
* **Supported Filesystems:**
  * `ext2`, `ext3`, `ext4` via `dump(8)` and `restore(8)`
  * `xfs` via `xfsdump(8)` and `xfsrestore(8)` (auto-detected on restore via the `xFSdUmP\0` stream magic header)
* **SSL / Crypto Engine:** Works with either system **OpenSSL** (`-chacha20`) or **LibreSSL** (`-chacha`, including automatic detection of `/opt/tardis/bin/openssl` + `/opt/tardis/lib`)
* **Dump State:** `/var/lib/dumpdates` and `/var/lib/xfsdump/inventory`

#### Installing Runtime Tools on Linux

Install `openssl` plus `dump` (for `ext2`/`ext3`/`ext4`) and/or `xfsdump` (for `xfs`):

~~sh
# Debian / Ubuntu
sudo apt update && sudo apt install -y openssl dump xfsdump

# RHEL / Rocky / AlmaLinux / Fedora
sudo dnf install -y openssl dump xfsdump

# Arch Linux
sudo pacman -S --needed openssl dump xfsdump

# Alpine Linux
sudo apk add openssl dump xfsdump
~~

> **Note on OpenSSL vs. LibreSSL Stream Interoperability:**  
> OpenSSL (`enc -chacha20`) and LibreSSL (`enc -chacha`) use different internal IV/counter layouts in `openssl enc`. Each host decrypts archives created with its own SSL provider (`OpenSSL` or `LibreSSL`). If you intend to browse or decrypt backups from both OpenBSD and Linux hosts interchangeably using a shared SSL format, install LibreSSL into `/opt/tardis` on your Linux hosts (see Section 4); Tardis on Linux automatically prefers `/opt/tardis/bin/openssl` when present.

---

## 2. Building from Source

### Build Requirements
* **Go 1.22 or newer** (`doas pkg_add go` on OpenBSD, or distro Go / official tarball on Linux)
* **Git**

### Native Build (OpenBSD or Linux)

`build.sh` automatically detects your host `GOOS` (`openbsd` or `linux`) and `GOARCH`, compiling a static `CGO_ENABLED=0` binary:

~~sh
git clone https://github.com/aladrin/tardis.git
cd tardis
sh ./build.sh
~~

### Cross-Compilation Matrix

Because Tardis uses `CGO_ENABLED=0`, pure-Go `zstd`, and `golang.org/x/sys/unix` ioctls, you can cross-compile release binaries between OpenBSD and Linux from any workstation:

~~sh
# Build for OpenBSD (amd64, arm64, mips64)
GOOS=openbsd GOARCH=amd64  sh ./build.sh
GOOS=openbsd GOARCH=arm64  sh ./build.sh
GOOS=openbsd GOARCH=mips64 sh ./build.sh

# Build for Linux (amd64, arm64)
GOOS=linux GOARCH=amd64 sh ./build.sh
GOOS=linux GOARCH=arm64 sh ./build.sh
~~

---

## 3. System Installation & Directory Layout

### OpenBSD Installation

~~sh
doas install -o root -g wheel -m 0755 tardis /usr/local/sbin/tardis
doas install -d -o root -g wheel -m 0700 /etc/tardis.d /var/db/tardis /var/db/tardis/jobs
~~

### Linux Installation

~~sh
sudo install -o root -g root -m 0755 tardis /usr/local/sbin/tardis
sudo install -d -o root -g root -m 0700 /etc/tardis.d /var/db/tardis /var/db/tardis/jobs
~~

### Filesystem Paths Used by Tardis

| Path | Mode | Purpose |
| :--- | :--- | :--- |
| `/usr/local/sbin/tardis` | `0755` | Main CLI and interactive terminal UI binary |
| `/etc/tardis.d/config.json` | `0600` | S3 bucket, region, endpoint, credentials, SSL paths, and default tier |
| `/etc/tardis.d/{client}.key` | `0400` | 128-hex-char master keyfile (256-bit encryption secret + 256-bit MAC key) |
| `/var/db/tardis/{client}/` | `0700` | Local `zstd` cache of HMAC-verified S3 `STANDARD` snapshot catalogs |
| `/var/db/tardis/jobs/` | `0700` | Persistent JSON state for queued and thawing Glacier restore jobs |
| `/var/run/tardis.lock` | `0600` | Kernel `flock(2)` lockfile preventing concurrent local dump/restore runs |
| `/var/log/tardis` | `0600` | Operational and cryptographic verification audit log |
| `/etc/dumpdates` | `0664` | OpenBSD `dump(8)` incremental level timestamps |
| `/var/lib/dumpdates` | `0664` | Linux `dump(8)` & `xfsdump(8)` incremental level timestamps |
| `/var/lib/xfsdump/inventory` | `0755` | Linux `xfsdump(8)` session/media inventory |

---

## 4. Optional: Building LibreSSL into `/opt/tardis` on Linux

On Linux, Tardis works immediately with your distribution's `/usr/bin/openssl` (OpenSSL 1.1.1 / 3.x). If you prefer LibreSSL on Linux—for example, to maintain 100% cipher compatibility with OpenBSD hosts—compile LibreSSL into `/opt/tardis`:

~~sh
LIBRESSL_VER="4.0.0"
curl -LO "https://ftp.openbsd.org/pub/OpenBSD/LibreSSL/libressl-${LIBRESSL_VER}.tar.gz"
tar xzf "libressl-${LIBRESSL_VER}.tar.gz"
cd "libressl-${LIBRESSL_VER}"

./configure --prefix=/opt/tardis --libdir=/opt/tardis/lib
make -j"$(nproc)"
sudo make install
~~

Tardis on Linux automatically probes `/opt/tardis/bin/openssl` and injects `LD_LIBRARY_PATH=/opt/tardis/lib` exclusively into the `openssl` subprocess environment. You can also override `openssl_bin` and `ld_library_path` explicitly in `/etc/tardis.d/config.json` or TUI View 4.

---

## 5. AWS S3 Bucket & IAM Policy Setup

1. Create a dedicated private S3 bucket with **Block All Public Access** enabled.
2. Attach the following least-privilege IAM policy to the backup user or instance role (replace `YOUR-BUCKET-NAME`):

~~json
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
~~

### S3 Object Layout per Snapshot
Each backup writes four objects under `s3://{bucket}/{client}/`:
* `{timestamp}_{client}{mount}_{level}.zst.dat` (`DEEP_ARCHIVE`) — Compressed, encrypted `dump` / `xfsdump` stream
* `{timestamp}_{client}{mount}_{level}.hmac` (`STANDARD`) — Domain-bound `HMAC-SHA256` authentication record for the archive
* `{timestamp}_{client}{mount}_{level}_list.zst.dat` (`STANDARD`) — Compressed, encrypted JSONL file/inode catalog
* `{timestamp}_{client}{mount}_{level}_list.hmac` (`STANDARD`) — Domain-bound `HMAC-SHA256` authentication record for the catalog

---

## 6. Configuration & Cryptographic Self-Test

### Option A: Interactive Terminal UI Setup (Recommended)

Launch Tardis as `root` (`doas` on OpenBSD, `sudo` on Linux):

~~sh
# OpenBSD
doas /usr/local/sbin/tardis

# Linux
sudo /usr/local/sbin/tardis
~~

When `/etc/tardis.d/config.json` is unconfigured, Tardis opens directly on **View 4 (`S3 & SYSTEM CONFIG`)**:
1. Use the Up/Down arrow keys and `Enter` to configure **S3 Bucket**, **AWS Region**, **Client Identifier**, and credentials (**AWS Shared Profile** or **Access Key ID** + **Secret Access Key**).
2. Select **`[ Generate / Verify Client 0400 Keyfile ]`** to atomically create `/etc/tardis.d/{client}.key` (`0400`) from OS CSPRNG.
3. Press **`t`** to run the live SSL `ChaCha20+PBKDF2+HMAC-SHA256` round-trip and S3 connectivity check.
4. Press **`s`** to atomically write `/etc/tardis.d/config.json` (`0600`).

### Option B: Manual `/etc/tardis.d/config.json`

Create `/etc/tardis.d/config.json` (`0600`):

~~json
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
  "default_tier": "Bulk",
  "openssl_bin": "",
  "ld_library_path": ""
}
~~

If `aws_profile`, `access_key_id`, and `secret_access_key` are empty, Tardis uses standard AWS SDK credential resolution (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, `/root/.aws/credentials`, or EC2/ECS IAM roles).

### Verify Platform Drivers, Crypto, & S3 (`-S`)

Run `-S` on each host after installation:

~~sh
sudo /usr/local/sbin/tardis -S   # or: doas /usr/local/sbin/tardis -S
~~

Example output on **OpenBSD**:

~~text
platform   : OpenBSD (dumpdates=/etc/dumpdates)
crypto     : PASS — LibreSSL 4.0.0 (/usr/bin/openssl, -chacha + PBKDF2-100k + HMAC-SHA256)
keyfile    : /etc/tardis.d/ibex.key [generated (0400), id=8f2a1c90e4b1]
ffs dump   : READY — /sbin/dump
ffs restore: READY — /sbin/restore
s3         : PASS — s3://your-tardis-backup-bucket/ibex/ reachable (ap-southeast-2)
~~

Example output on **Linux**:

~~text
platform   : Linux (dumpdates=/var/lib/dumpdates)
crypto     : PASS — OpenSSL 3.0.15 3 Sep 2024 (/usr/bin/openssl, -chacha20 + PBKDF2-100k + HMAC-SHA256)
keyfile    : /etc/tardis.d/nettle.key [verified, id=4d91bc07a213]
ext dump   : READY — /usr/sbin/dump
ext restore: READY — /usr/sbin/restore
xfsdump    : READY — /usr/sbin/xfsdump
xfsrestore : READY — /usr/sbin/xfsrestore
s3         : PASS — s3://your-tardis-backup-bucket/nettle/ reachable (ap-southeast-2)
~~

**CRITICAL — OFFLINE KEY BACKUP:** Copy `/etc/tardis.d/{client}.key` to secure offline storage (such as an encrypted vault or hardware token). Without this 128-character hex keyfile, archives in S3 cannot be decrypted or authenticated.

---

## 7. Automated Backups & Glacier Thaw Polling (`cron`)

Inspect supported filesystems on your host before scheduling backups:

~~sh
# OpenBSD (FFS partitions)
mount -t ffs

# Linux (ext2/ext3/ext4 and XFS partitions)
findmnt -t ext2,ext3,ext4,xfs
~~

Add tiered backup schedules and periodic Glacier restore execution to `root`'s crontab (`doas crontab -e` or `sudo crontab -e`):

~~cron
# Monthly Level 0 full dumps on the 1st of the month
0  2  1  *  *   /usr/local/sbin/tardis -m / -d 0
15 2  1  *  *   /usr/local/sbin/tardis -m /var -d 0
30 2  1  *  *   /usr/local/sbin/tardis -m /srv/data -d 0

# Weekly Level 1 dumps every Sunday (2nd-31st)
0  3  2-31 * 0  /usr/local/sbin/tardis -m / -d 1
15 3  2-31 * 0  /usr/local/sbin/tardis -m /var -d 1
30 3  2-31 * 0  /usr/local/sbin/tardis -m /srv/data -d 1

# Daily Level 2 incremental dumps Monday-Saturday (exits cleanly if no inodes changed)
0  4  *  * 1-6  /usr/local/sbin/tardis -m / -d 2
10 4  *  * 1-6  /usr/local/sbin/tardis -m /var -d 2
20 4  *  * 1-6  /usr/local/sbin/tardis -m /srv/data -d 2

# Poll queued/thawing S3 Glacier Deep Archive restore jobs every 30 minutes
*/30 * * * *    /usr/local/sbin/tardis -J
~~

---

## 8. Interactive UI & CLI Reference

### Interactive Terminal UI (`tardis` or `tardis -u`)

* **Global Keys**: `Left` / `Right` arrows (or `Tab`) switch across the 4 views; `q` or `Ctrl+C` exits cleanly.
* **View 1 — Dump Timeline**:
  * `1`–`9`: Quick-filter by mountpoint (`0` shows `ALL`).
  * `/`: Regex or substring filter across snapshots.
  * `c`: Cycle or enter another client hostname prefix in the bucket.
  * `t` / `T`: Toggle default Glacier tier (`Bulk` / `Standard`) or submit a thaw request (`T`) for the highlighted archive.
  * `D`: Delete the highlighted snapshot bundle (requires `y` confirmation).
  * `Enter`: Load the snapshot's S3 `STANDARD` catalog into the Snapshot Browser.
* **View 2 — Snapshot Browser & Time-Travel**:
  * `[` and `]`: Step **older** (`[`) or **newer** (`]`) across dump dates for the active mountpoint while keeping your cursor anchored on the current file/folder and updating the `[ VERSIONS ]` strip.
  * `p`: Preload and verify all S3 `STANDARD` catalogs for the active mountpoint so `[ VERSIONS ]` populates across all dates.
  * `Enter`: Expand/collapse directories (or jump to the highlighted item inside the tree when a `/` filter is active).
  * `Space` / `u`: Toggle `[✓]` restore marks on individual files or entire directory subtrees (`u` clears all marks).
  * `m` / `o`: Switch between **OVERWRITE** and **NEW_PATH** restore modes (`o` sets the alternate filename, target directory, or suffix such as `.restored`).
  * `x` / `X`: Queue and start a staged, HMAC-verified restore (`restore` or `xfsrestore`) for the active snapshot (`x`) or the full chronological `L0 -> Ln` dump chain (`X`).
* **View 3 — Jobs & Glacier Monitor**:
  * `r`: Poll S3 `HeadObject` thaw status and run ready jobs immediately.
  * `R` / `d`: Retry a failed job (`R`) or remove a finished job record (`d`).
* **View 4 — S3 & System Config**:
  * `Enter` / `Space`: Edit S3, keyfile, or SSL binary/`LD_LIBRARY_PATH` fields; `t` runs self-tests; `s` saves `/etc/tardis.d/config.json` (`0600`).

### Headless CLI Commands

~~sh
# Dump /srv/xfsdata (XFS on Linux) or /home (FFS on OpenBSD / ext4 on Linux) at Level 0
tardis -m /srv/xfsdata -d 0

# Override SSL binary on the command line if testing a custom build
tardis -O /opt/tardis/bin/openssl -S

# Query S3 objects and live Glacier thaw status (FROZEN / THAWING / READY)
tardis -q ALL

# Restore a full archive to an alternate directory (auto-detects restore vs xfsrestore)
tardis -m /srv/xfsdata -r 2026-11-14_02:30:00_nettle_srv_xfsdata_0.zst.dat -t Standard -o /srv/xfsdata/restore_test

# Delete an obsolete snapshot bundle (archive, catalog, and both .hmac files)
tardis -D 2026-11-14_02:30:00_nettle_srv_xfsdata_0
~~

---

## 9. Emergency Bare-Metal Recovery (Without `tardis` Binary)

Every archive can be manually authenticated and extracted using standard OS utilities (`openssl`, `zstd`, and `restore` or `xfsrestore`).

### Step 1: Verify Domain-Bound `HMAC-SHA256` Before Touching Disk

~~sh
CLIENT="nettle"
ARCHIVE="2026-11-14_02:30:00_nettle_home_0.zst.dat"
S3_OBJ_KEY="${CLIENT}/${ARCHIVE}"
KEYFILE="/etc/tardis.d/${CLIENT}.key"

MAC_HEX=$(tail -c 65 "$KEYFILE" | tr -d '\r\n')
( printf 'TARDIS-V2\0%s\0' "$S3_OBJ_KEY"; cat "$ARCHIVE" ) | \
  openssl dgst -sha256 -mac HMAC -macopt hexkey:"$MAC_HEX"
~~

Confirm the resulting hex digest matches the `.hmac` file **before** extracting.

### Step 2A: Decrypt & Extract on OpenBSD (`ffs` + LibreSSL)

~~sh
mkdir -p /mnt/recovery && cd /mnt/recovery
head -c 64 "$KEYFILE" | \
  /usr/bin/openssl enc -chacha -pbkdf2 -iter 100000 -d -pass stdin -in "/tmp/${ARCHIVE}" | \
  zstd -d | /sbin/restore -rf -
~~

### Step 2B: Decrypt & Extract on Linux (`ext2/3/4` or `xfs`)

Set `CIPHER="-chacha20"` when decrypting with OpenSSL, or `CIPHER="-chacha"` when decrypting with LibreSSL:

~~sh
CIPHER="-chacha20"   # use "-chacha" if encrypted with LibreSSL
mkdir -p /mnt/recovery

# For ext2 / ext3 / ext4 dump archives:
cd /mnt/recovery
head -c 64 "$KEYFILE" | \
  openssl enc "$CIPHER" -pbkdf2 -iter 100000 -d -pass stdin -in "/tmp/${ARCHIVE}" | \
  zstd -d | restore -rf -

# For XFS xfsdump archives:
head -c 64 "$KEYFILE" | \
  openssl enc "$CIPHER" -pbkdf2 -iter 100000 -d -pass stdin -in "/tmp/${ARCHIVE}" | \
  zstd -d | xfsrestore -F -J - /mnt/recovery
~~
