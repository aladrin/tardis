# Incremental backup to Amazon S3

## Dependancies
### OpenBSD
```
pkg_add py3-tqdm awscli dateutils zstd
```
### Ubuntu
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
