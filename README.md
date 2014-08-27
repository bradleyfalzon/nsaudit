NS Audit
=======

Load a list of domain names and verify check their NS settings (according to TLDs, not their own NS records)

Usage
=====

Add domains (one per line) to a file in the local directory called domains.txt.

nsaudit's only arguments are a list of name servers to ensure each domain has.

$ go build
$ nsaudit ns1.example.com ns2.example.com
