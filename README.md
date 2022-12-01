# Incremental direct backup to Amazon S3
This backs up and encrypts directly over the wire. No staging required. No unencrypted data over SSL that can leak to man in the middle.
When run with the dump option, the following files will be transferred to Amazon S3:
* File system dump: zstandard compression, chacha encryption, saved to deep archive
* File listing of files in the dump, not encrypted
* SHA256 of the dump file
## Dependencies
### OpenBSD
Works on ffs filesystem.
```
pkg_add py3-tqdm awscli dateutils zstd
```
### Ubuntu
Works on ext2, ext3, ext4 and btrfs filesystems.
I have no interest in adding OpenSSL support at this stage.
You will have to download and compile LibreSSL https://www.libressl.org/
```
apt install -y awscli dump python3-tqdm dateutils zstd
```

```
usage: tardis -b bucket [-c client] [-k key] [-m mount [-d level|-r file [-t Standard|Bulk]]] [-q ALL|STANDARD|DEEP_ARCHIVE]
options: -r (restore)
         -d (dump)
         -D (delete)
         -q (query)
         -t tier Standard (3-5 hours)
         -t tier Bulk (5-12 hours)
```
