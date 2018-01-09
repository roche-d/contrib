/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A small utility program to lookup hostnames of endpoints in a service.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	pollPeriod = 1 * time.Second
)

var (
	onChange   = flag.String("on-change", "", "Script to run on change, must accept a new line separated list of peers via stdin.")
	onStart    = flag.String("on-start", "", "Script to run on start, must accept a new line separated list of peers via stdin.")
	svc        = flag.String("service", "", "Governing service responsible for the DNS records of the domain this pod is in.")
	namespace  = flag.String("ns", "", "The namespace this pod is running in. If unspecified, the POD_NAMESPACE env var is used.")
	domain     = flag.String("domain", "", "The Cluster Domain which is used by the Cluster, if not set tries to determine it from /etc/resolv.conf file.")
	extDomains = flag.String("extdomain", "", "Comma-separated list of additional domains to probe (multi cluster peer finding).")
)

func lookup(svcNames []string) (sets.String, error) {
	endpoints := sets.NewString()
	for _, svcName := range svcNames {
		_, srvRecords, err := net.LookupSRV("", "", svcName)
		if err != nil {
			return endpoints, err
		}
		for _, srvRecord := range srvRecords {
			// The SRV records ends in a "." for the root domain
			ep := fmt.Sprintf("%v", srvRecord.Target[:len(srvRecord.Target)-1])
			endpoints.Insert(ep)
		}
	}
	return endpoints, nil
}

func shellOut(sendStdin, script string) {
	log.Printf("execing: %v with stdin: %v", script, sendStdin)
	// TODO: Switch to sending stdin from go
	out, err := exec.Command("bash", "-c", fmt.Sprintf("echo -e '%v' | %v", sendStdin, script)).CombinedOutput()
	if err != nil {
		log.Fatalf("Failed to execute %v: %v, err: %v", script, string(out), err)
	}
	log.Print(string(out))
}

func main() {
	flag.Parse()

	ns := *namespace
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("Failed to get hostname: %s", err)
	}
	var domainNames = []string{""}

	// If domain is not provided, try to get it from resolv.conf
	if *domain == "" {
		resolvConfBytes, err := ioutil.ReadFile("/etc/resolv.conf")
		resolvConf := string(resolvConfBytes)
		if err != nil {
			log.Fatal("Unable to read /etc/resolv.conf")
		}

		var re *regexp.Regexp
		if ns == "" {
			// Looking for a domain that looks like with *.svc.**
			re, err = regexp.Compile(`\A(.*\n)*search\s{1,}(.*\s{1,})*(?P<goal>[a-zA-Z0-9-]{1,63}.svc.([a-zA-Z0-9-]{1,63}\.)*[a-zA-Z0-9]{2,63})`)
		} else {
			// Looking for a domain that looks like svc.**
			re, err = regexp.Compile(`\A(.*\n)*search\s{1,}(.*\s{1,})*(?P<goal>svc.([a-zA-Z0-9-]{1,63}\.)*[a-zA-Z0-9]{2,63})`)
		}
		if err != nil {
			log.Fatalf("Failed to create regular expression: %v", err)
		}

		groupNames := re.SubexpNames()
		result := re.FindStringSubmatch(resolvConf)
		for k, v := range result {
			if groupNames[k] == "goal" {
				if ns == "" {
					// Domain is complete if ns is empty
					domainNames = []string{v}
				} else {
					// Need to convert svc.** into ns.svc.**
					domainNames = []string{ns + "." + v}
				}
				break
			}
		}
		log.Printf("Determined Domain to be %s", domainNames[0])

	} else {
		domainNames = []string{strings.Join([]string{ns, "svc", *domain}, ".")}
	}

	if *svc == "" || domainNames[0] == "" || (*onChange == "" && *onStart == "") {
		log.Fatalf("Incomplete args, require -on-change and/or -on-start, -service and -ns or an env var for POD_NAMESPACE.")
	}

	if *extDomains != "" {
		for _, d := range strings.Split(*extDomains, ",") {
			if d != "" {
				if strings.HasSuffix(d, ".local") {
					domainNames = append(domainNames, strings.Join([]string{ns, "svc", d}, "."))
				} else {
					domainNames = append(domainNames, strings.Join([]string{ns, "svc", d, "local"}, "."))
				}
			}
		}
	}
	log.Printf("Following domains will be searched %v", domainNames)

	myName := strings.Join([]string{hostname, *svc, domainNames[0]}, ".")

	script := *onStart
	if script == "" {
		script = *onChange
		log.Printf("No on-start supplied, on-change %v will be applied on start.", script)
	}

	var services []string
	for _, domain := range domainNames {
		services = append(services, strings.Join([]string{*svc, domain}, "."))
	}

	for newPeers, peers := sets.NewString(), sets.NewString(); script != ""; time.Sleep(pollPeriod) {
		newPeers, err = lookup(services)
		if err != nil {
			log.Printf("%v", err)
			continue
		}
		if newPeers.Equal(peers) || !newPeers.Has(myName) {
			log.Printf("Have not found myself in list yet.\nMy Hostname: %s\nHosts in list: %s", myName, strings.Join(newPeers.List(), ", "))
			continue
		}
		peerList := newPeers.List()
		sort.Strings(peerList)
		log.Printf("Peer list updated\nwas %v\nnow %v", peers.List(), newPeers.List())
		shellOut(strings.Join(peerList, "\n"), script)
		peers = newPeers
		script = *onChange
	}

	// TODO: Exit if there's no on-change?
	log.Printf("Peer finder exiting")
}
