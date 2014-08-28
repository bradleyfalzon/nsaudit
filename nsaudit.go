package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/deckarep/golang-set"
	"github.com/droundy/goopt"
	"github.com/miekg/dns"
)

var (
	nsCache = make(map[string]string)
)

type DomainNS struct {
	Domain string
	Error  error
	RegistrarNS,
	ZoneNS mapset.Set
}

var argsFile = goopt.String([]string{"-f", "--file"}, "domains.csv", "Read domains from this file")
var argsNS = goopt.Strings([]string{"-n", "--nameserver"}, "", "Name server to check for (use option multiple times)")
var argsCB = goopt.Int([]string{"-c", "--channel-buffer"}, 4096, "Size of the golang channel buffer, must be larger than number of domains")
var argsW = goopt.Int([]string{"-w", "--workers"}, 10, "Concurrent workers to start to fetch DNS records")
var argsTO = goopt.Int([]string{"-t", "--timeout"}, 5, "DNS timeout in seconds")
var argsRE = goopt.Int([]string{"-r", "--retry"}, 3, "DNS retry times before giving up")

func main() {

	goopt.Parse(nil)

	requiredNS := mapset.NewSet()
	for _, ns := range *argsNS {
		ns := strings.TrimRight(ns, ".") + "."
		requiredNS.Add(ns)
	}

	if requiredNS.Cardinality() == 0 {
		log.Fatalln("Name servers not set, see --help")
	}

	log.Printf("Loaded, checking for name servers: %v\n", requiredNS)

	domains, err := os.Open(*argsFile)
	if err != nil {
		log.Fatal(err)
	}
	defer domains.Close()

	// Create our buffered channel
	inChan := make(chan string, *argsCB)
	outChan := make(chan DomainNS, *argsCB)

	// Insert domains into buffered channel, we do this as a go func in case
	// we're inserting more records than the channel has buffers. Once a buffer
	// is full, we'd block until it starts draining - and we can't start
	// draining if we block whilst filling it.
	go func() {
		scanner := bufio.NewScanner(domains)
		for scanner.Scan() {
			// write the domain to the channel for processing
			inChan <- scanner.Text()
		}
		log.Println("Finished adding domains to channel")
	}()

	var wg sync.WaitGroup

	for i := 0; i < *argsW; i++ {
		log.Println("Starting worker:", i)

		wg.Add(1)
		go func(wg *sync.WaitGroup) {

			defer wg.Done()
			for {
				select {

				case domain := <-inChan:
					domainNS, err := checkDomain(domain)
					if err != nil {
						log.Println("Error processing domain:", err)
					}
					outChan <- domainNS
				default:
					return
				}
			}
		}(&wg)
	}

	log.Println("Waiting for workers to finish")
	wg.Wait()

	// Close the channel, so when the channel is empty (we've read it all) we
	// don't block waiting for more data. Instead channel will return empty
	// type, and we can detect this.
	close(outChan)

	totalDomains := 0
	totalErrors := 0
	domainsWithErrors := 0

	fmt.Println()
	done := false
	for {
		select {
		case domainNS, ok := <-outChan:
			if ok {
				totalDomains++
				errors := compareNS(requiredNS, domainNS)
				if errors > 0 {
					totalErrors += errors
					domainsWithErrors++
				}
			} else {
				// Empty struct, finishing
				done = true
			}
		default:
			break
		}

		if done {
			break
		}
	}

	fmt.Printf("\nStats\n-----\n")
	fmt.Printf("Domains: %d\n", totalDomains)
	fmt.Printf("Domains with Errors/Warnings: %d (%.0f%%)\n", domainsWithErrors, float64(domainsWithErrors)/float64(totalDomains)*100)
	fmt.Printf("Domains without Errors/Warnings: %d (%.0f%%)\n", totalDomains-domainsWithErrors, float64(totalDomains-domainsWithErrors)/float64(totalDomains)*100)
	fmt.Printf("Total Errors: %d\n", totalErrors)

}

func compareNS(requiredNS mapset.Set, domainNS DomainNS) (errors int) {

	fmt.Printf("----- %s -----\n", domainNS.Domain)
	errors = 0

	if domainNS.Error != nil {
		fmt.Println("CRIT:", domainNS.Error)
		errors++
		return
	}

	requiredVregistrar := requiredNS.Difference(domainNS.RegistrarNS)
	if requiredVregistrar.Cardinality() > 0 {
		fmt.Println("ERROR: Required, not in registrar:", requiredVregistrar)
		errors++
	}

	registrarVrequired := domainNS.RegistrarNS.Difference(requiredNS)
	if registrarVrequired.Cardinality() > 0 {
		fmt.Println("ERROR: In registrar, not required:", registrarVrequired)
		errors++
	}

	zoneVregistrar := domainNS.ZoneNS.Difference(domainNS.RegistrarNS)
	if zoneVregistrar.Cardinality() > 0 {
		fmt.Println("WARN: In zone, not in registrar:", zoneVregistrar)
		errors++
	}

	registrarVzone := domainNS.RegistrarNS.Difference(domainNS.ZoneNS)
	if registrarVzone.Cardinality() > 0 {
		fmt.Println("WARN: In registrar, not in zone:", registrarVzone)
		errors++
	}

	if errors == 0 {
		fmt.Println("OK")
	}

	return

}

func checkDomain(domain string) (domainNS DomainNS, err error) {

	// I don't actually know if this is required, might make LookupNS faster as
	// it knows it's rooted already
	if domain[len(domain):] != "." {
		domain = domain + "."
	}
	domainNS.Domain = domain

	parent, parentNS, zoneNS, err := domainParent(domain)
	if err != nil {
		domainNS.Error = err
		return
	}
	log.Printf("Domain: %s, Parent: %s, ParentNS: %s", domain, parent, parentNS)

	log.Println("Fetching registrar NS records for domain:", domain)
	domainNS.RegistrarNS, err = queryNS(domain, parentNS, true)
	if err != nil {
		return
	}

	log.Println("Fetching zone NS records for domain:", domain)
	domainNS.ZoneNS, err = queryNS(domain, zoneNS, false)
	if err != nil {
		return
	}

	return
}

func queryNS(domain, nameServer string, checkNS bool) (set mapset.Set, err error) {
	r, err := query(domain, nameServer)
	if err != nil {
		return
	}

	if r.Rcode != dns.RcodeSuccess {
		log.Printf("%#v\n", r)
		err = errors.New(fmt.Sprintf("Bad response for domain:%s", domain))
		return
	}

	set = mapset.NewSet()
	//log.Printf("%#v\n", r)

	var check *[]dns.RR
	if checkNS {
		check = &r.Ns
	} else {
		check = &r.Answer
	}

	for _, a := range *check {
		if ns, ok := a.(*dns.NS); ok {
			set.Add(ns.Ns)
		}
	}

	return

}

func query(domain, parentNS string) (r *dns.Msg, err error) {
	m := new(dns.Msg)
	m.SetQuestion(domain, dns.TypeNS)

	for i := 1; i <= *argsRE; i++ {
		c := dns.Client{DialTimeout: time.Duration(*argsTO) * time.Second}
		r, _, err = c.Exchange(m, parentNS+":53")
		if err == nil {
			return
		}
	}

	return nil, errors.New(fmt.Sprintf("Too many retries looking up NS records for domain %s to server %s, last error: %s", domain, parentNS, err))

}

func domainParent(domain string) (parent, parentNS, zoneNS string, err error) {

	domainParts := strings.Split(domain, ".")
	parent = strings.Join(domainParts[1:], ".")

	zoneNSs, err := net.LookupNS(domain)
	if err != nil {
		return
	}
	if len(zoneNSs) == 0 {
		err = errors.New(fmt.Sprintf("Could not find NS for domain %s", domain))
		return
	}
	zoneNS = zoneNSs[0].Host

	var ok bool
	if parentNS, ok = nsCache[parent]; ok {
		log.Println("Loaded parent NS from cache")
		return
	}

	// Parent NS (eg .com.au, .net) not found in cache

	parentNSs, err := net.LookupNS(parent)
	if err != nil {
		return
	}

	if len(parentNSs) == 0 {
		err = errors.New(fmt.Sprintf("Could not find NS for domains's tld %s", parent))
		return
	}

	parentNS = parentNSs[0].Host
	nsCache[parent] = parentNS

	return
}
