NS Audit
=======

Load a list of domain names and verify check their NS settings (according to TLDs, not their own NS records)

Usage
=====

Add domains (one per line) to a file called domains.txt, and use `-n` option to specify the name servers required.

```
$ go build
$ nsaudit -n ns1.example.com -n ns2.example.com -f domains.txt
```

```
Usage of ./nsaudit:
Options:
  -f domains.csv  --file=domains.csv     Read domains from this file
  -n              --nameserver=          Name server to check for (use option multiple times)
  -c 4096         --channel-buffer=4096  Size of the golang channel buffer, should (must?) be larger than number of domains
  -w 10           --workers=10           Maximum number of concurrent workers to start to fetch DNS records
                  --help                 show usage message
```
