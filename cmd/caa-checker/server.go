package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"sync"

	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/miekg/dns"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/gopkg.in/yaml.v2"

	"github.com/letsencrypt/boulder/bdns"
	"github.com/letsencrypt/boulder/cmd"
	pb "github.com/letsencrypt/boulder/cmd/caa-checker/proto"
	"github.com/letsencrypt/boulder/metrics"
)

type caaCheckerServer struct {
	issuer   string
	resolver bdns.DNSResolver
}

// caaSet consists of filtered CAA records
type caaSet struct {
	Issue     []*dns.CAA
	Issuewild []*dns.CAA
	Iodef     []*dns.CAA
	Unknown   []*dns.CAA
}

// returns true if any CAA records have unknown tag properties and are flagged critical.
func (caaSet caaSet) criticalUnknown() bool {
	if len(caaSet.Unknown) > 0 {
		for _, caaRecord := range caaSet.Unknown {
			// The critical flag is the bit with significance 128. However, many CAA
			// record users have misinterpreted the RFC and concluded that the bit
			// with significance 1 is the critical bit. This is sufficiently
			// widespread that that bit must reasonably be considered an alias for
			// the critical bit. The remaining bits are 0/ignore as proscribed by the
			// RFC.
			if (caaRecord.Flag & (128 | 1)) != 0 {
				return true
			}
		}
	}

	return false
}

// Filter CAA records by property
func newCAASet(CAAs []*dns.CAA) *caaSet {
	var filtered caaSet

	for _, caaRecord := range CAAs {
		switch caaRecord.Tag {
		case "issue":
			filtered.Issue = append(filtered.Issue, caaRecord)
		case "issuewild":
			filtered.Issuewild = append(filtered.Issuewild, caaRecord)
		case "iodef":
			filtered.Iodef = append(filtered.Iodef, caaRecord)
		default:
			filtered.Unknown = append(filtered.Unknown, caaRecord)
		}
	}

	return &filtered
}

func (ccs *caaCheckerServer) getCAASet(ctx context.Context, hostname string) (*caaSet, error) {
	hostname = strings.TrimRight(hostname, ".")
	labels := strings.Split(hostname, ".")

	// See RFC 6844 "Certification Authority Processing" for pseudocode.
	// Essentially: check CAA records for the FDQN to be issued, and all
	// parent domains.
	//
	// The lookups are performed in parallel in order to avoid timing out
	// the RPC call.
	//
	// We depend on our resolver to snap CNAME and DNAME records.

	type result struct {
		records []*dns.CAA
		err     error
	}
	results := make([]result, len(labels))

	var wg sync.WaitGroup

	for i := 0; i < len(labels); i++ {
		// Start the concurrent DNS lookup.
		wg.Add(1)
		go func(name string, r *result) {
			r.records, r.err = ccs.resolver.LookupCAA(ctx, hostname)
			wg.Done()
		}(strings.Join(labels[i:], "."), &results[i])
	}

	wg.Wait()

	// Return the first result
	for _, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		if len(res.records) > 0 {
			return newCAASet(res.records), nil
		}
	}

	// no CAA records found
	return nil, nil
}

// Given a CAA record, assume that the Value is in the issue/issuewild format,
// that is, a domain name with zero or more additional key-value parameters.
// Returns the domain name, which may be "" (unsatisfiable).
func extractIssuerDomain(caa *dns.CAA) string {
	v := caa.Value
	v = strings.Trim(v, " \t") // Value can start and end with whitespace.
	idx := strings.IndexByte(v, ';')
	if idx < 0 {
		return v // no parameters; domain only
	}

	// Currently, ignore parameters. Unfortunately, the RFC makes no statement on
	// whether any parameters are critical. Treat unknown parameters as
	// non-critical.
	return strings.Trim(v[0:idx], " \t")
}

func (ccs *caaCheckerServer) checkCAA(ctx context.Context, hostname string) (bool, error) {
	hostname = strings.ToLower(hostname)
	caaSet, err := ccs.getCAASet(ctx, hostname)
	if err != nil {
		return false, err
	}

	if caaSet == nil {
		// No CAA records found, can issue
		return true, nil
	}

	if caaSet.criticalUnknown() {
		// Contains unknown critical directives.
		return false, nil
	}

	if len(caaSet.Issue) == 0 {
		// Although CAA records exist, none of them pertain to issuance in this case.
		// (e.g. there is only an issuewild directive, but we are checking for a
		// non-wildcard identifier, or there is only an iodef or non-critical unknown
		// directive.)
		return true, nil
	}

	// There are CAA records pertaining to issuance in our case. Note that this
	// includes the case of the unsatisfiable CAA record value ";", used to
	// prevent issuance by any CA under any circumstance.
	//
	// Our CAA identity must be found in the chosen checkSet.
	for _, caa := range caaSet.Issue {
		if extractIssuerDomain(caa) == ccs.issuer {
			return true, nil
		}
	}

	// The list of authorized issuers is non-empty, but we are not in it. Fail.
	return false, nil
}

func (ccs *caaCheckerServer) ValidForIssuance(ctx context.Context, domain *pb.Domain) (*pb.Valid, error) {
	valid, err := ccs.checkCAA(ctx, domain.Name)
	if err != nil {
		return nil, err
	}
	return &pb.Valid{valid}, nil
}

type config struct {
	Address      string             `yaml:"address"`
	DNSResolver  string             `yaml:"dns-resolver"`
	DNSNetwork   string             `yaml:"dns-network"`
	DNSTimeout   cmd.ConfigDuration `yaml:"dns-timeout"`
	IssuerDomain string             `yaml:"issuer-domain"`
}

func main() {
	configPath := flag.String("config", "config.yml", "Path to configuration file")
	flag.Parse()

	configBytes, err := ioutil.ReadFile(*configPath)
	cmd.FailOnError(err, fmt.Sprintf("Failed to read configuration file from '%s'", *configPath))
	var c config
	err = yaml.Unmarshal(configBytes, &c)
	cmd.FailOnError(err, fmt.Sprintf("Failed to parse configuration file from '%s'", *configPath))

	l, err := net.Listen("tcp", c.Address)
	cmd.FailOnError(err, fmt.Sprintf("Failed to listen on '%s'", c.Address))
	s := grpc.NewServer()
	resolver := bdns.NewDNSResolverImpl(
		c.DNSTimeout.Duration,
		[]string{c.DNSResolver},
		metrics.NewNoopScope(),
		clock.Default(),
		5,
	)
	ccs := &caaCheckerServer{c.IssuerDomain, resolver}
	pb.RegisterCAACheckerServer(s, ccs)
	err = s.Serve(l)
	cmd.FailOnError(err, "gRPC service failed")
}