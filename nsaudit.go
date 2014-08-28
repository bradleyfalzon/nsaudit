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
	"time"

	"github.com/deckarep/golang-set"
	"github.com/miekg/dns"
)

var nsCache = make(map[string]string)

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

	scanner := bufio.NewScanner(domains)
	for scanner.Scan() {
		domain := scanner.Text()
		registrarNS, zoneNS, err := checkDomain(domain, flag.Args())
		if err != nil {
			log.Println("Error processing domain:", err)
			continue
		}

		compareNS(domain, requiredNS, registrarNS, zoneNS)

	}

}

func compareNS(domain string, requiredNS, registrarNS, zoneNS mapset.Set) {

	//log.Print(registrarNS)
	//log.Print(zoneNS)

	log.Println("Processing domain:", domain)

	zoneVregistrar := zoneNS.Difference(registrarNS)
	if zoneVregistrar.Cardinality() > 0 {
		log.Println("WARNING the following entries are in the zone, but not in the registrar")
		log.Println(zoneVregistrar)
	}

	registrarVzone := registrarNS.Difference(zoneNS)
	if registrarVzone.Cardinality() > 0 {
		log.Println("WARNING, the following entries are in the registrar, but not in the zone")
		log.Println(registrarVzone)
	}

	requiredVregistrar := requiredNS.Difference(registrarNS)
	if requiredVregistrar.Cardinality() > 0 {
		log.Println("WARNING the following entries are required, but not in the registrar")
		log.Println(requiredVregistrar)
	}

	registrarVrequired := registrarNS.Difference(requiredNS)
	if registrarVrequired.Cardinality() > 0 {
		log.Println("WARNING, the following entries are in the registrar, but not required")
		log.Println(registrarVrequired)
	}

}

func checkDomain(domain string, requireNS []string) (registrarNSRR mapset.Set, zoneNSRR mapset.Set, err error) {

	// I don't actually know if this is required, might make LookupNS faster as
	// it knows it's rooted already
	if domain[len(domain):] != "." {
		domain = domain + "."
	}

	parent, parentNS, zoneNS, err := domainParent(domain)
	if err != nil {
		return
	}
	log.Printf("Domain: %s, Parent: %s, ParentNS: %s", domain, parent, parentNS)

	log.Println("Fetching registrar NS records...")
	registrarNSRR, err = queryNS(domain, parentNS, true)

	log.Println("Fetching zone NS records...")
	zoneNSRR, err = queryNS(domain, zoneNS, false)

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
