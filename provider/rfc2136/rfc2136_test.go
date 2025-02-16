/*
Copyright 2017 The Kubernetes Authors.

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

package rfc2136

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

type rfc2136Stub struct {
	output     []*dns.Envelope
	updateMsgs []*dns.Msg
	createMsgs []*dns.Msg
}

func newStub() *rfc2136Stub {
	return &rfc2136Stub{
		output:     make([]*dns.Envelope, 0),
		updateMsgs: make([]*dns.Msg, 0),
		createMsgs: make([]*dns.Msg, 0),
	}
}

func getSortedChanges(msgs []*dns.Msg) []string {
	r := []string{}
	for _, d := range msgs {
		// only care about section after the ZONE SECTION: as the id: needs stripped out in order to sort and grantee the order when sorting
		r = append(r, strings.Split(d.String(), "ZONE SECTION:")[1])
	}
	sort.Strings(r)
	return r
}

func (r *rfc2136Stub) SendMessage(msg *dns.Msg) error {
	zone := extractZoneFromMessage(msg.String())
	// Make sure the zone starts with . to make sure HasSuffix does not match forbar.com for zone bar.com
	if !strings.HasPrefix(zone, ".") {
		zone = "." + zone
	}
	log.Infof("zone=%s", zone)
	lines := extractUpdateSectionFromMessage(msg)
	for _, line := range lines {
		// break at first empty line
		if len(strings.TrimSpace(line)) == 0 {
			break
		}

		line = strings.Replace(line, "\t", " ", -1)
		log.Info(line)
		record := strings.Split(line, " ")[0]
		if !strings.HasSuffix(record, zone) {
			err := fmt.Errorf("Message contains updates outside of it's zone.  zone=%v record=%v", zone, record)
			log.Error(err)
			return err
		}

		if strings.Contains(line, " NONE ") {
			r.updateMsgs = append(r.updateMsgs, msg)
		} else if strings.Contains(line, " IN ") {
			r.createMsgs = append(r.createMsgs, msg)
		}
	}

	return nil
}

func (r *rfc2136Stub) setOutput(output []string) error {
	r.output = make([]*dns.Envelope, len(output))
	for i, e := range output {
		rr, err := dns.NewRR(e)
		if err != nil {
			return err
		}
		r.output[i] = &dns.Envelope{
			RR: []dns.RR{rr},
		}
	}
	return nil
}

func (r *rfc2136Stub) IncomeTransfer(m *dns.Msg, a string) (env chan *dns.Envelope, err error) {
	outChan := make(chan *dns.Envelope)
	go func() {
		for _, e := range r.output {
			outChan <- e
		}
		close(outChan)
	}()

	return outChan, nil
}

func createRfc2136StubProvider(stub *rfc2136Stub) (provider.Provider, error) {
	return NewRfc2136Provider("", 0, nil, false, "key", "secret", "hmac-sha512", true, endpoint.DomainFilter{}, false, 300*time.Second, false, "", "", "", 50, stub)
}

func createRfc2136StubProviderWithZones(stub *rfc2136Stub) (provider.Provider, error) {
	zones := []string{"foo.com", "foobar.com"}
	return NewRfc2136Provider("", 0, zones, false, "key", "secret", "hmac-sha512", true, endpoint.DomainFilter{}, false, 300*time.Second, false, "", "", "", 50, stub)
}

func createRfc2136StubProviderWithZonesFilters(stub *rfc2136Stub) (provider.Provider, error) {
	zones := []string{"foo.com", "foobar.com"}
	return NewRfc2136Provider("", 0, zones, false, "key", "secret", "hmac-sha512", true, endpoint.DomainFilter{Filters: zones}, false, 300*time.Second, false, "", "", "", 50, stub)
}

func extractUpdateSectionFromMessage(msg fmt.Stringer) []string {
	const searchPattern = "UPDATE SECTION:"
	updateSectionOffset := strings.Index(msg.String(), searchPattern)
	return strings.Split(strings.TrimSpace(msg.String()[updateSectionOffset+len(searchPattern):]), "\n")
}

func extractZoneFromMessage(msg string) string {
	re := regexp.MustCompile(`ZONE SECTION:\n;(?P<ZONE>[\.,\-,\w,\d]+)\t`)
	matches := re.FindStringSubmatch(msg)
	return matches[re.SubexpIndex("ZONE")]
}

// TestRfc2136GetRecordsMultipleTargets simulates a single record with multiple targets.
func TestRfc2136GetRecordsMultipleTargets(t *testing.T) {
	stub := newStub()
	err := stub.setOutput([]string{
		"foo.com 3600 IN A 1.1.1.1",
		"foo.com 3600 IN A 2.2.2.2",
	})
	assert.NoError(t, err)

	provider, err := createRfc2136StubProvider(stub)
	assert.NoError(t, err)

	recs, err := provider.Records(context.Background())
	assert.NoError(t, err)

	assert.Equal(t, 1, len(recs), "expected single record")
	assert.Equal(t, recs[0].DNSName, "foo.com")
	assert.Equal(t, 2, len(recs[0].Targets), "expected two targets")
	assert.True(t, recs[0].Targets[0] == "1.1.1.1" || recs[0].Targets[1] == "1.1.1.1") // ignore order
	assert.True(t, recs[0].Targets[0] == "2.2.2.2" || recs[0].Targets[1] == "2.2.2.2") // ignore order
	assert.Equal(t, recs[0].RecordType, "A")
	assert.Equal(t, recs[0].RecordTTL, endpoint.TTL(3600))
	assert.Equal(t, 0, len(recs[0].Labels), "expected no labels")
	assert.Equal(t, 0, len(recs[0].ProviderSpecific), "expected no provider specific config")
}

func TestRfc2136GetRecords(t *testing.T) {
	stub := newStub()
	err := stub.setOutput([]string{
		"v4.barfoo.com 3600 TXT test1",
		"v1.foo.com 3600 TXT test2",
		"v2.bar.com 3600 A 8.8.8.8",
		"v3.bar.com 3600 TXT bbbb",
		"v2.foo.com 3600 CNAME cccc",
		"v1.foobar.com 3600 TXT dddd",
	})
	assert.NoError(t, err)

	provider, err := createRfc2136StubProvider(stub)
	assert.NoError(t, err)

	recs, err := provider.Records(context.Background())
	assert.NoError(t, err)

	assert.Equal(t, 6, len(recs))
	assert.True(t, contains(recs, "v1.foo.com"))
	assert.True(t, contains(recs, "v2.bar.com"))
	assert.True(t, contains(recs, "v2.foo.com"))
}

// Make sure the test version of SendMessage raises an error
// if a zone update ever contains records outside of it's zone
// as the TestRfc2136ApplyChanges tests all assume this
func TestRfc2136SendMessage(t *testing.T) {
	stub := newStub()

	m := new(dns.Msg)
	m.SetUpdate("foo.com.")
	rr, err := dns.NewRR(fmt.Sprintf("%s %d %s %s", "v1.foo.com.", 0, "A", "1.2.3.4"))
	m.Insert([]dns.RR{rr})

	err = stub.SendMessage(m)
	assert.NoError(t, err)

	rr, err = dns.NewRR(fmt.Sprintf("%s %d %s %s", "v1.bar.com.", 0, "A", "1.2.3.4"))
	m.Insert([]dns.RR{rr})

	err = stub.SendMessage(m)
	assert.Error(t, err)

	m.SetUpdate(".")
	err = stub.SendMessage(m)
	assert.NoError(t, err)
}

// These tests are use the . root zone with no filters
func TestRfc2136ApplyChanges(t *testing.T) {
	stub := newStub()
	provider, err := createRfc2136StubProvider(stub)
	assert.NoError(t, err)

	p := &plan.Changes{
		Create: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v1.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom"},
			},
			{
				DNSName:    "ns.foobar.com",
				RecordType: "NS",
				Targets:    []string{"boom"},
			},
		},
		Delete: []*endpoint.Endpoint{
			{
				DNSName:    "v2.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
			},
			{
				DNSName:    "v2.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom2"},
			},
		},
	}

	err = provider.ApplyChanges(context.Background(), p)
	assert.NoError(t, err)

	assert.Equal(t, 3, len(stub.createMsgs))
	assert.True(t, strings.Contains(stub.createMsgs[0].String(), "v1.foo.com"))
	assert.True(t, strings.Contains(stub.createMsgs[0].String(), "1.2.3.4"))

	assert.True(t, strings.Contains(stub.createMsgs[1].String(), "v1.foobar.com"))
	assert.True(t, strings.Contains(stub.createMsgs[1].String(), "boom"))

	assert.True(t, strings.Contains(stub.createMsgs[2].String(), "ns.foobar.com"))
	assert.True(t, strings.Contains(stub.createMsgs[2].String(), "boom"))

	assert.Equal(t, 2, len(stub.updateMsgs))
	assert.True(t, strings.Contains(stub.updateMsgs[0].String(), "v2.foo.com"))
	assert.True(t, strings.Contains(stub.updateMsgs[1].String(), "v2.foobar.com"))
}

// These tests all use the foo.com and foobar.com zones with no filters
// createMsgs and updateMsgs need sorted when are are used
func TestRfc2136ApplyChangesWithZones(t *testing.T) {
	stub := newStub()
	provider, err := createRfc2136StubProviderWithZones(stub)
	assert.NoError(t, err)

	p := &plan.Changes{
		Create: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v1.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom"},
			},
			{
				DNSName:    "ns.foobar.com",
				RecordType: "NS",
				Targets:    []string{"boom"},
			},
		},
		Delete: []*endpoint.Endpoint{
			{
				DNSName:    "v2.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
			},
			{
				DNSName:    "v2.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom2"},
			},
		},
	}

	err = provider.ApplyChanges(context.Background(), p)
	assert.NoError(t, err)

	assert.Equal(t, 3, len(stub.createMsgs))
	createMsgs := getSortedChanges(stub.createMsgs)
	assert.Equal(t, 3, len(createMsgs))

	assert.True(t, strings.Contains(createMsgs[0], "v1.foo.com"))
	assert.True(t, strings.Contains(createMsgs[0], "1.2.3.4"))

	assert.True(t, strings.Contains(createMsgs[1], "v1.foobar.com"))
	assert.True(t, strings.Contains(createMsgs[1], "boom"))

	assert.True(t, strings.Contains(createMsgs[2], "ns.foobar.com"))
	assert.True(t, strings.Contains(createMsgs[2], "boom"))

	assert.Equal(t, 2, len(stub.updateMsgs))
	updateMsgs := getSortedChanges(stub.updateMsgs)
	assert.Equal(t, 2, len(updateMsgs))

	assert.True(t, strings.Contains(updateMsgs[0], "v2.foo.com"))
	assert.True(t, strings.Contains(updateMsgs[1], "v2.foobar.com"))
}

// These tests use the foo.com and foobar.com zones and with filters set to both zones
// createMsgs and updateMsgs need sorted when are are used
func TestRfc2136ApplyChangesWithZonesFilters(t *testing.T) {
	stub := newStub()
	provider, err := createRfc2136StubProviderWithZonesFilters(stub)
	assert.NoError(t, err)

	p := &plan.Changes{
		Create: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v1.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom"},
			},
			{
				DNSName:    "ns.foobar.com",
				RecordType: "NS",
				Targets:    []string{"boom"},
			},
			{
				DNSName:    "filtered-out.foo.bar",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
				RecordTTL:  endpoint.TTL(400),
			},
		},
		Delete: []*endpoint.Endpoint{
			{
				DNSName:    "v2.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
			},
			{
				DNSName:    "v2.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom2"},
			},
		},
	}

	err = provider.ApplyChanges(context.Background(), p)
	assert.NoError(t, err)

	assert.Equal(t, 3, len(stub.createMsgs))
	createMsgs := getSortedChanges(stub.createMsgs)
	assert.Equal(t, 3, len(createMsgs))

	assert.True(t, strings.Contains(createMsgs[0], "v1.foo.com"))
	assert.True(t, strings.Contains(createMsgs[0], "1.2.3.4"))

	assert.True(t, strings.Contains(createMsgs[1], "v1.foobar.com"))
	assert.True(t, strings.Contains(createMsgs[1], "boom"))

	assert.True(t, strings.Contains(createMsgs[2], "ns.foobar.com"))
	assert.True(t, strings.Contains(createMsgs[2], "boom"))

	for _, s := range createMsgs {
		assert.False(t, strings.Contains(s, "filtered-out.foo.bar"))
	}

	assert.Equal(t, 2, len(stub.updateMsgs))
	updateMsgs := getSortedChanges(stub.updateMsgs)
	assert.Equal(t, 2, len(updateMsgs))

	assert.True(t, strings.Contains(updateMsgs[0], "v2.foo.com"))
	assert.True(t, strings.Contains(updateMsgs[1], "v2.foobar.com"))

}

func TestRfc2136ApplyChangesWithDifferentTTLs(t *testing.T) {
	stub := newStub()

	provider, err := createRfc2136StubProvider(stub)
	assert.NoError(t, err)

	p := &plan.Changes{
		Create: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"2.1.1.1"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v2.foo.com",
				RecordType: "A",
				Targets:    []string{"3.2.2.2"},
				RecordTTL:  endpoint.TTL(200),
			},
			{
				DNSName:    "v3.foo.com",
				RecordType: "A",
				Targets:    []string{"4.3.3.3"},
			},
		},
	}

	err = provider.ApplyChanges(context.Background(), p)
	assert.NoError(t, err)

	createRecords := extractUpdateSectionFromMessage(stub.createMsgs[0])
	assert.Equal(t, 3, len(createRecords))
	assert.True(t, strings.Contains(createRecords[0], "v1.foo.com"))
	assert.True(t, strings.Contains(createRecords[0], "2.1.1.1"))
	assert.True(t, strings.Contains(createRecords[0], "400"))
	assert.True(t, strings.Contains(createRecords[1], "v2.foo.com"))
	assert.True(t, strings.Contains(createRecords[1], "3.2.2.2"))
	assert.True(t, strings.Contains(createRecords[1], "300"))
	assert.True(t, strings.Contains(createRecords[2], "v3.foo.com"))
	assert.True(t, strings.Contains(createRecords[2], "4.3.3.3"))
	assert.True(t, strings.Contains(createRecords[2], "300"))
}

func TestRfc2136ApplyChangesWithUpdate(t *testing.T) {
	stub := newStub()

	provider, err := createRfc2136StubProvider(stub)
	assert.NoError(t, err)

	p := &plan.Changes{
		Create: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v1.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom"},
			},
		},
	}

	err = provider.ApplyChanges(context.Background(), p)
	assert.NoError(t, err)

	p = &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.4"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v1.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"boom"},
			},
		},
		UpdateNew: []*endpoint.Endpoint{
			{
				DNSName:    "v1.foo.com",
				RecordType: "A",
				Targets:    []string{"1.2.3.5"},
				RecordTTL:  endpoint.TTL(400),
			},
			{
				DNSName:    "v1.foobar.com",
				RecordType: "TXT",
				Targets:    []string{"kablui"},
			},
		},
	}

	err = provider.ApplyChanges(context.Background(), p)
	assert.NoError(t, err)

	assert.Equal(t, 4, len(stub.createMsgs))
	assert.Equal(t, 2, len(stub.updateMsgs))

	assert.True(t, strings.Contains(stub.createMsgs[0].String(), "v1.foo.com"))
	assert.True(t, strings.Contains(stub.createMsgs[0].String(), "1.2.3.4"))
	assert.True(t, strings.Contains(stub.createMsgs[2].String(), "v1.foo.com"))
	assert.True(t, strings.Contains(stub.createMsgs[2].String(), "1.2.3.5"))

	assert.True(t, strings.Contains(stub.updateMsgs[0].String(), "v1.foo.com"))
	assert.True(t, strings.Contains(stub.updateMsgs[0].String(), "1.2.3.4"))

	assert.True(t, strings.Contains(stub.createMsgs[1].String(), "v1.foobar.com"))
	assert.True(t, strings.Contains(stub.createMsgs[1].String(), "boom"))
	assert.True(t, strings.Contains(stub.createMsgs[3].String(), "v1.foobar.com"))
	assert.True(t, strings.Contains(stub.createMsgs[3].String(), "kablui"))

	assert.True(t, strings.Contains(stub.updateMsgs[1].String(), "v1.foobar.com"))
	assert.True(t, strings.Contains(stub.updateMsgs[1].String(), "boom"))
}

func TestChunkBy(t *testing.T) {
	var records []*endpoint.Endpoint

	for i := 0; i < 10; i++ {
		records = append(records, &endpoint.Endpoint{
			DNSName:    "v1.foo.com",
			RecordType: "A",
			Targets:    []string{"1.1.2.2"},
			RecordTTL:  endpoint.TTL(400),
		})
	}

	chunks := chunkBy(records, 2)
	if len(chunks) != 5 {
		t.Errorf("incorrect number of chunks returned")
	}
}

func contains(arr []*endpoint.Endpoint, name string) bool {
	for _, a := range arr {
		if a.DNSName == name {
			return true
		}
	}
	return false
}
