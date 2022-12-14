# Incremental direct backup to Amazon S3
This backs up with end-to-end encryption (E2EE). No staging required. No unencrypted data over SSL that can leak to man in the middle.
When run with the dump option, the following files will be transferred to Amazon S3:
* File system dump: zstandard compression, ChaCha20 stream cipher, saved to deep archive
* File listing of files in the dump, not encrypted
* SHA256 of the dump file
## Dependencies
### OpenBSD
Works on ffs filesystem.
```
pkg_add py3-tqdm awscli dateutils zstd git
mkdir -p /opt/tardis/bin
git clone https://github.com/aladrin/tardis.git
mv tardis/tardis /opt/tardis/bin
rm -rf tardis
```
### Linux
Works on ext2, ext3, ext4 and btrfs filesystems.
#### Docker
```
docker pull aladrin/tardis
docker run --name tardis -i -t aladrin/tardis
```
#### Ubuntu
You will have to download and compile LibreSSL https://www.libressl.org/
```
apt install -y awscli dump python3-tqdm dateutils zstd git gcc curl make
curl https://cloudflare.cdn.openbsd.org/pub/OpenBSD/LibreSSL/libressl-$(curl -s https://www.libressl.org/ |fgrep relnotes.txt|awk -F'relnotes.txt">' '{print $2}'|awk -F'<' '{print $1}'|awk '{print $NF}').tar.gz -O
tar xzvf libressl-*.tar.gz
rm libressl*.tar.gz
cd libressl*
./configure --prefix=/opt/tardis
make install
cd ..
rm -rf libressl*
git clone https://github.com/aladrin/tardis.git
mv tardis/tardis /opt/tardis/bin
rm -rf tardis

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
