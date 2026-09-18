package dnssd

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestBuildRecordsIncludesIPPAndLegacyServices(t *testing.T) {
	records, err := BuildRecords(ServiceInput{
		Name:     "Office Printer",
		Hostname: "proxy.example.test",
		Port:     8631,
		TXT: map[string]string{
			"product": "golieipp",
			"ty":      "Office",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ServiceRecord{
		{Name: "Office Printer", Hostname: "proxy.example.test", Port: 8631, Type: "_ipp._tcp", TXT: map[string]string{"product": "golieipp", "ty": "Office"}},
		{Name: "Office Printer", Hostname: "proxy.example.test", Port: 8631, Type: "_ipp._tcp", Subtype: "_print"},
		{Name: "Office Printer", Hostname: "proxy.example.test", Port: 8631, Type: "_ipp._tcp", Subtype: "_universal"},
		{Name: "Office Printer", Hostname: "proxy.example.test", Port: 0, Type: "_printer._tcp"},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
	if got := records[1].FullType(); got != "_print._sub._ipp._tcp" {
		t.Fatalf("print subtype service type = %q", got)
	}
	if got := records[2].FullType(); got != "_universal._sub._ipp._tcp" {
		t.Fatalf("universal subtype service type = %q", got)
	}
}

func TestBuildRecordsIncludesIPPSVariantsWhenRequested(t *testing.T) {
	records, err := BuildRecords(ServiceInput{
		Name:     "Secure Printer",
		Hostname: "proxy.example.test",
		Port:     443,
		IPPS:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, record := range records {
		types = append(types, record.FullType())
	}
	want := []string{
		"_ipp._tcp",
		"_print._sub._ipp._tcp",
		"_universal._sub._ipp._tcp",
		"_printer._tcp",
		"_ipps._tcp",
		"_print._sub._ipps._tcp",
		"_universal._sub._ipps._tcp",
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("service types = %#v, want %#v", types, want)
	}
	if records[3].Port != 0 {
		t.Fatalf("legacy service port = %d, want 0", records[3].Port)
	}
}

func TestBuildRecordsForProfilesPublishesOnlyReadyPlaintextProfiles(t *testing.T) {
	records, err := BuildRecordsForProfiles(ServiceInput{
		Name:     "Secure Printer",
		Hostname: "proxy.example.test",
		Port:     631,
		IPPS:     true,
		TXT: map[string]string{
			"rp":    "printers/office",
			"pdl":   "application/pdf,image/urf",
			"URF":   "V1.4,W8,SRGB24,RS300",
			"Color": "F",
		},
	}, PublicationProfiles{Ordinary: true, IPPEverywhere: true})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, record := range records {
		types = append(types, record.FullType())
		if strings.HasPrefix(record.Type, "_ipps") {
			t.Fatalf("plaintext profile publication emitted secure service: %#v", record)
		}
	}
	want := []string{"_ipp._tcp", "_print._sub._ipp._tcp", "_printer._tcp"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("profile service types = %#v, want %#v", types, want)
	}
	if records[len(records)-1].Port != 0 {
		t.Fatalf("legacy _printer._tcp port = %d, want 0", records[len(records)-1].Port)
	}
}

func TestBuildRecordsForProfilesDropsOnlyProfileWithOversizedEssentialTXT(t *testing.T) {
	records, err := BuildRecordsForProfiles(ServiceInput{
		Name:     "Office",
		Hostname: "proxy.example.test",
		Port:     631,
		TXT: map[string]string{
			"rp":  "printers/office",
			"pdl": "application/pdf",
			"URF": strings.Repeat("x", 260),
		},
	}, PublicationProfiles{Ordinary: true, AirPrint: true, IPPEverywhere: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.FullType() == "_universal._sub._ipp._tcp" {
			t.Fatalf("AirPrint subtype survived an oversized URF essential: %#v", records)
		}
	}
	if len(records) != 3 || records[0].FullType() != "_ipp._tcp" || records[1].FullType() != "_print._sub._ipp._tcp" || records[2].FullType() != "_printer._tcp" {
		t.Fatalf("unrelated profiles were not retained: %#v", records)
	}
}

func TestBuildRecordsRejectsInvalidInput(t *testing.T) {
	tests := []ServiceInput{
		{Name: "", Hostname: "proxy.example.test", Port: 8631},
		{Name: "Office", Hostname: "proxy.example.test", Port: 0},
	}
	for _, input := range tests {
		if _, err := BuildRecords(input); err == nil {
			t.Fatalf("BuildRecords(%+v) unexpectedly succeeded", input)
		}
	}
}

func TestTXTEntriesPrioritizeRoutingAndVersionKeys(t *testing.T) {
	entries := TXTEntries(map[string]string{
		"product": "golieipp",
		"URF":     "V1.5,W8,SRGB24,RS600",
		"pdl":     "application/pdf",
		"rp":      "printers/office",
		"qtotal":  "1",
		"txtvers": "1",
	})
	want := []string{
		"rp=printers/office",
		"txtvers=1",
		"qtotal=1",
		"product=golieipp",
		"pdl=application/pdf",
		"URF=V1.5,W8,SRGB24,RS600",
	}
	if len(entries) != len(want) {
		t.Fatalf("TXT entry count = %d, want %d: %#v", len(entries), len(want), entries)
	}
	for index := range want {
		if entries[index] != want[index] {
			t.Fatalf("TXT entry %d = %q, want %q", index, entries[index], want[index])
		}
	}
}

func TestBuildRecordsRejectsOversizedTXTData(t *testing.T) {
	longValue := strings.Repeat("x", 240)
	records, err := BuildRecords(ServiceInput{
		Name:     "Office Printer",
		Hostname: "proxy.example.test",
		Port:     8631,
		TXT: map[string]string{
			"pdl":      longValue,
			"URF":      longValue,
			"product":  longValue,
			"note":     longValue,
			"adminurl": longValue,
			"ty":       longValue,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds 1300") || records != nil {
		t.Fatalf("oversized TXT data was accepted: records=%#v err=%v", records, err)
	}
}

func TestEncodeLOCProvidesRFC1876RDataAndGeoInputPath(t *testing.T) {
	data, err := EncodeLOC("geo:50.0755,14.4378,250")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 16 || data[0] != 0 {
		t.Fatalf("LOC RDATA = %#v, want 16-byte version-zero payload", data)
	}
	records, err := BuildRecords(ServiceInput{
		Name:        "Office",
		Port:        8631,
		GeoLocation: "geo:50.0755,14.4378",
	})
	if err != nil {
		t.Fatal(err)
	}
	if records[0].GeoLocation != "geo:50.0755,14.4378" {
		t.Fatalf("geo location was not carried by the record input path: %#v", records[0])
	}
}

func TestAlternateServiceNameIsBoundedAndDeterministic(t *testing.T) {
	if got := AlternateServiceName("Office", 0); got != "Office" {
		t.Fatalf("attempt zero = %q", got)
	}
	if got := AlternateServiceName("Office", 1); got != "Office (2)" {
		t.Fatalf("attempt one = %q", got)
	}
	if got := AlternateServiceName("Office", 3); got != "Office (4)" {
		t.Fatalf("attempt three = %q", got)
	}
	gotNames := CollisionNames("Office", 3)
	wantNames := []string{"Office", "Office (2)", "Office (3)"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("collision names = %#v, want %#v", gotNames, wantNames)
	}
}

func TestStubPublisherPublishesUpdatesAtomicallyAndCanBecomeDegraded(t *testing.T) {
	publisher := NewStubPublisher()
	first := ServiceInput{Name: "Office", Hostname: "proxy.example.test", Port: 8631}
	status, err := publisher.Publish(context.Background(), first)
	if err != nil || status.State != StatePublished {
		t.Fatalf("initial publish status=%+v err=%v", status, err)
	}
	second := first
	second.Name = "Office New"
	status, err = publisher.Update(context.Background(), second)
	if err != nil || status.State != StatePublished || status.Name != "Office New" {
		t.Fatalf("update status=%+v err=%v", status, err)
	}
	current, ok := publisher.Current()
	if !ok || current.Name != "Office New" {
		t.Fatalf("current publication = %+v, ok=%v", current, ok)
	}

	publisher.SetAvailable(false)
	third := second
	third.Name = "Unavailable Update"
	status, err = publisher.Update(context.Background(), third)
	if err != nil || status.State != StateDegraded {
		t.Fatalf("degraded update status=%+v err=%v", status, err)
	}
	current, ok = publisher.Current()
	if !ok || current.Name != "Office New" {
		t.Fatalf("failed update replaced current publication: %+v, ok=%v", current, ok)
	}

	publisher.SetAvailable(true)
	status, err = publisher.Update(context.Background(), third)
	if err != nil || status.State != StatePublished {
		t.Fatalf("recovered update status=%+v err=%v", status, err)
	}
	status, err = publisher.Withdraw(context.Background())
	if err != nil || status.State != StateWithdrawn {
		t.Fatalf("withdraw status=%+v err=%v", status, err)
	}
	if _, ok := publisher.Current(); ok {
		t.Fatal("publication remained after withdraw")
	}
}
