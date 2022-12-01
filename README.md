# Incremental backup to Amazon S3

### Dependancies
## Ubuntu
```
apt install -y dump python3-tqdm dateutils zstd
```
* Use at own risk. Please assume this is currently broken.

```
usage: tardis [-c client] [-b bucket] [-m mount [-d level|-r file [-t Standard|Bulk]]] [-q ALL|STANDARD|DEEP_ARCHIVE]
options: -r (restore)
         -d (dump)
         -D (delete)
         -q (query)
         -t tier Standard (3-5 hours)
         -t tier Bulk (5-12 hours)
```
