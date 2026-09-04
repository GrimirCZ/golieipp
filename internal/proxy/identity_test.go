package proxy

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

func TestProxyPrinterUUIDIsStableAndQueueSpecific(t *testing.T) {
	officeURI := "ipp://proxy.example/printers/office"
	first := proxyPrinterUUID(officeURI)
	second := proxyPrinterUUID(officeURI)
	if first != second {
		t.Fatalf("proxy UUID changed between calls: %q != %q", first, second)
	}
	if first == proxyPrinterUUID("ipp://proxy.example/printers/home") {
		t.Fatal("different proxy queues received the same UUID")
	}
	if first == proxyPrinterUUID("ipp://other-proxy.example/printers/office") {
		t.Fatal("different proxy URIs received the same UUID")
	}
	if !strings.HasPrefix(first, "urn:uuid:") || len(first) != len("urn:uuid:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx") {
		t.Fatalf("UUID is not an IPP UUID URI: %q", first)
	}
	uuidText := strings.TrimPrefix(first, "urn:uuid:")
	if uuidText[14] != '5' {
		t.Fatalf("UUID is not version 5: %q", first)
	}
	if !strings.Contains("89ab", strings.ToLower(uuidText[19:20])) {
		t.Fatalf("UUID does not use the RFC 4122 variant: %q", first)
	}
}

func TestFilterPrinterAttributesReplacesUpstreamIdentityAndUsesIPPTypes(t *testing.T) {
	upstreamDate := time.Date(2026, time.August, 26, 9, 12, 13, 0, time.FixedZone("CEST", 2*60*60))
	upstream := goipp.Attributes{
		iattr.URI("printer-uuid", "urn:uuid:c2354b94-1111-5111-8111-111111111111"),
		iattr.Integer("printer-up-time", 987654),
		iattr.Integer("printer-config-change-time", 12345),
		goipp.MakeAttribute("printer-config-change-date-time", goipp.TagDateTime, goipp.Time{Time: upstreamDate}),
		iattr.Keywords("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in"),
	}
	printer := config.PrinterConfig{
		DisplayName: "Dílny",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	configDate := time.Date(2026, time.September, 4, 9, 39, 20, 0, time.UTC)
	metadata := PrinterIdentityMetadata{
		UUID:                 proxyPrinterUUID("ipp://proxy.example/printers/dilny"),
		Uptime:               42,
		ConfigChangeTime:     7,
		ConfigChangeDateTime: configDate,
	}
	out := FilterPrinterAttributes(upstream, "dilny", "ipp://proxy.example/printers/dilny", printer, metadata)

	for _, name := range []string{"printer-uuid", "printer-up-time", "printer-config-change-time", "printer-config-change-date-time"} {
		attr, ok := iattr.Attr(out, name)
		if !ok || len(attr.Values) != 1 {
			t.Fatalf("missing generated %s: %+v", name, attr)
		}
	}
	uuidAttr, _ := iattr.Attr(out, "printer-uuid")
	if uuidAttr.Values[0].T != goipp.TagURI || uuidAttr.Values[0].V != goipp.String(metadata.UUID) {
		t.Fatalf("printer-uuid has wrong IPP type/value: %+v", uuidAttr.Values[0])
	}
	uptimeAttr, _ := iattr.Attr(out, "printer-up-time")
	if uptimeAttr.Values[0].T != goipp.TagInteger || uptimeAttr.Values[0].V != goipp.Integer(metadata.Uptime) {
		t.Fatalf("printer-up-time has wrong IPP type/value: %+v", uptimeAttr.Values[0])
	}
	changeAttr, _ := iattr.Attr(out, "printer-config-change-time")
	if changeAttr.Values[0].T != goipp.TagInteger || changeAttr.Values[0].V != goipp.Integer(metadata.ConfigChangeTime) {
		t.Fatalf("printer-config-change-time has wrong IPP type/value: %+v", changeAttr.Values[0])
	}
	dateAttr, _ := iattr.Attr(out, "printer-config-change-date-time")
	if dateAttr.Values[0].T != goipp.TagDateTime {
		t.Fatalf("printer-config-change-date-time has wrong IPP tag: %+v", dateAttr.Values[0])
	}
	date, ok := dateAttr.Values[0].V.(goipp.Time)
	if !ok || !date.Time.Equal(configDate) {
		t.Fatalf("printer-config-change-date-time has wrong value: %#v", dateAttr.Values[0].V)
	}
	if got, _ := iattr.FirstString(out, "printer-uuid"); got == "urn:uuid:c2354b94-1111-5111-8111-111111111111" {
		t.Fatal("upstream printer UUID leaked through the proxy")
	}
}

func TestFilterPrinterAttributesPreventsMacOSUpstreamCapabilityCacheReuse(t *testing.T) {
	proxyURI := "ipp://192.168.200.5:8631/printers/dilny"
	printer := config.PrinterConfig{
		DisplayName: "IPP Dílny",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	upstream := goipp.Attributes{
		iattr.URI("printer-uuid", "urn:uuid:c2354b94-4df7-4f8d-9cae-0c7b8c4b9999"),
		iattr.Integer("printer-up-time", 123456),
		iattr.Integer("printer-config-change-time", 70000),
		goipp.MakeAttribute("printer-config-change-date-time", goipp.TagDateTime, goipp.Time{Time: time.Date(2026, time.August, 26, 8, 0, 0, 0, time.UTC)}),
		iattr.Keywords("media-supported", "iso_a4_210x297mm", "iso_a3_297x420mm"),
	}
	metadata := PrinterIdentityMetadata{
		UUID:                 proxyPrinterUUID(proxyURI),
		Uptime:               10,
		ConfigChangeDateTime: time.Date(2026, time.September, 4, 9, 39, 20, 0, time.UTC),
	}
	first := FilterPrinterAttributes(upstream, "dilny", proxyURI, printer, metadata)
	// A later upstream refresh must not change the proxy identity or make its
	// newly advertised policy inherit the physical printer's media catalog.
	upstream = append(upstream, iattr.URI("printer-uuid", "urn:uuid:different-canon-identity"))
	second := FilterPrinterAttributes(upstream, "dilny", proxyURI, printer, metadata)
	firstUUID, _ := iattr.FirstString(first, "printer-uuid")
	secondUUID, _ := iattr.FirstString(second, "printer-uuid")
	if firstUUID != metadata.UUID || secondUUID != metadata.UUID || firstUUID != secondUUID {
		t.Fatalf("proxy identity was not isolated from upstream identity: %q %q", firstUUID, secondUUID)
	}
	if iattr.HasStringValue(second, "media-supported", "iso_a3_297x420mm") {
		t.Fatal("upstream-only media leaked into the macOS-facing capabilities")
	}
	firstChange, _ := iattr.FirstInt(first, "printer-config-change-time")
	secondChange, _ := iattr.FirstInt(second, "printer-config-change-time")
	if firstChange != 0 || secondChange != 0 {
		t.Fatalf("upstream config-change time leaked: %d %d", firstChange, secondChange)
	}
}

func TestServicePrinterIdentityIsStableAndUptimeNondecreasing(t *testing.T) {
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy.example/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {
				UpstreamURI: "ipp://upstream.example/printers/office",
				DisplayName: "Office",
				Policy: config.PolicyConfig{
					Media:          "iso_a4_210x297mm",
					MediaType:      "stationery",
					PrintColorMode: "monochrome",
				},
			},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.capabilities["office"] = goipp.Attributes{iattr.Keywords("media-supported", "iso_a4_210x297mm")}
	svc.mu.Unlock()

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
	printer := cfg.Printers["office"]
	firstResponse, err := svc.handleGetPrinterAttributes(context.Background(), "office", printer, req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	secondResponse, err := svc.handleGetPrinterAttributes(context.Background(), "office", printer, req)
	if err != nil {
		t.Fatal(err)
	}
	firstUptime, ok := iattr.FirstInt(firstResponse.Printer, "printer-up-time")
	if !ok {
		t.Fatal("first response omitted printer-up-time")
	}
	secondUptime, ok := iattr.FirstInt(secondResponse.Printer, "printer-up-time")
	if !ok || secondUptime < firstUptime {
		t.Fatalf("printer-up-time went backwards: %d -> %d", firstUptime, secondUptime)
	}
	firstChange, _ := iattr.FirstInt(firstResponse.Printer, "printer-config-change-time")
	secondChange, _ := iattr.FirstInt(secondResponse.Printer, "printer-config-change-time")
	if firstChange != secondChange || firstChange > firstUptime || secondChange > secondUptime {
		t.Fatalf("inconsistent proxy change time: uptime=%d/%d change=%d/%d", firstUptime, secondUptime, firstChange, secondChange)
	}
	firstUUID, _ := iattr.FirstString(firstResponse.Printer, "printer-uuid")
	secondUUID, _ := iattr.FirstString(secondResponse.Printer, "printer-uuid")
	if firstUUID != secondUUID || firstUUID != proxyPrinterUUID(svc.proxyPrinterURI("office")) {
		t.Fatalf("proxy UUID changed across capability queries: %q -> %q", firstUUID, secondUUID)
	}
	firstDate, _ := iattr.Attr(firstResponse.Printer, "printer-config-change-date-time")
	secondDate, _ := iattr.Attr(secondResponse.Printer, "printer-config-change-date-time")
	if firstDate.Values[0].T != goipp.TagDateTime || secondDate.Values[0].T != goipp.TagDateTime {
		t.Fatal("proxy config-change-date-time is not an IPP dateTime")
	}
	if firstDate.Values[0].V != secondDate.Values[0].V {
		t.Fatal("proxy config-change-date-time changed between queries")
	}
}
