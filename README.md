# Incremental backup to Amazon S3

* The incremental backup uses dump.
```
Ubuntu: apt install dump
OpenBSD: native
```

* The encryption uses OpenSSL.

* The compression uses zstd.
```
Ubuntu: apt install zstd
OpenBSD: pkg_add zstd
```

* Use at own risk. Currently under development.

```
usage: tardis [-c client] [-b bucket] [-m mount [-d level|-r file [-t Standard|Bulk]]] [-q ALL|STANDARD|DEEP_ARCHIVE]
options: -r (restore)
         -d (dump)
         -D (delete)
         -q (query)
         -t tier Standard (3-5 hours)
         -t tier Bulk (5-12 hours)
```
