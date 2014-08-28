package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/deckarep/golang-set"
	"github.com/miekg/dns"
)

var (
	nsCache       = make(map[string]string)
	channelBuffer = 4096
)

type DomainNS struct {
	Domain string
	RegistrarNS,
	ZoneNS mapset.Set
}

func main() {

	flag.Parse()

	if len(flag.Args()) == 0 {
		log.Fatalln("List all name servers as arguments.")
	}

	log.Printf("Loaded, checking for name servers: %v\n", flag.Args())

	requiredNS := mapset.NewSet()
	for _, ns := range flag.Args() {
		requiredNS.Add(ns)
	}

	domains, err := os.Open("domains.txt")
	if err != nil {
		log.Fatal(err)
	}
	defer domains.Close()

	// Create our buffered channel
	inChan := make(chan string, channelBuffer)
	outChan := make(chan DomainNS, channelBuffer)

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

	for i := 0; i < 4; i++ {
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
						continue
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

	log.Println("Results:")
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
	fmt.Printf("Domains with Errors/Warnings: %d (%d%%)\n", domainsWithErrors, domainsWithErrors/totalDomains*100)
	fmt.Printf("Domains without Errors/Warnings: %d (%d%%)\n", totalDomains-domainsWithErrors, (totalDomains-domainsWithErrors)/totalDomains*100)
	fmt.Printf("Total Errors: %d\n", totalErrors)

}

func compareNS(requiredNS mapset.Set, domainNS DomainNS) (errors int) {

	fmt.Printf("----- %s -----\n", domainNS.Domain)
	errors = 0

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

	requiredVregistrar := requiredNS.Difference(domainNS.RegistrarNS)
	if requiredVregistrar.Cardinality() > 0 {
		fmt.Println("ERROR: Required, not in registrar:", requiredVregistrar)
		errors++
	}

	registrarVrequired := domainNS.RegistrarNS.Difference(requiredNS)
	if registrarVrequired.Cardinality() > 0 {
		fmt.Printf("ERROR: In registrar, not required: %v\n", registrarVrequired)
		errors++
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
		return
	}
	log.Printf("Domain: %s, Parent: %s, ParentNS: %s", domain, parent, parentNS)

	log.Println("Fetching registrar NS records...")
	domainNS.RegistrarNS, err = queryNS(domain, parentNS, true)
	if err != nil {
		return
	}

	log.Println("Fetching zone NS records...")
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

	for i := 1; i <= 3; i++ {
		c := dns.Client{DialTimeout: time.Duration(i*5) * time.Second}
		r, _, err = c.Exchange(m, parentNS+":53")
		if err == nil {
			return
		}
	}

	return nil, errors.New("Too many retries, given up")

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
