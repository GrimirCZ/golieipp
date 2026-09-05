package dnssd

import (
	"context"
	"reflect"
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
		{Name: "Office Printer", Hostname: "proxy.example.test", Port: 0, Type: "_printer._tcp"},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("records = %#v, want %#v", records, want)
	}
	if got := records[1].FullType(); got != "_print._sub._ipp._tcp" {
		t.Fatalf("subtype service type = %q", got)
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
		"_printer._tcp",
		"_ipps._tcp",
		"_print._sub._ipps._tcp",
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("service types = %#v, want %#v", types, want)
	}
	if records[2].Port != 0 {
		t.Fatalf("legacy service port = %d, want 0", records[2].Port)
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
