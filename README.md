# Incremental backup to Amazon S3

## Dependancies
### Ubuntu
```
apt install -y dump python3-tqdm dateutils zstd
```
* Please assume this broken for the time being.

```
usage: tardis [-c client] [-b bucket] [-m mount [-d level|-r file [-t Standard|Bulk]]] [-q ALL|STANDARD|DEEP_ARCHIVE]
options: -r (restore)
         -d (dump)
         -D (delete)
         -q (query)
         -t tier Standard (3-5 hours)
         -t tier Bulk (5-12 hours)
```
