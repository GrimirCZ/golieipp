package proxy

import (
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestPrinterDNSSDTXTDerivedFromFilteredCapabilities(t *testing.T) {
	attrs := goipp.Attributes{
		iattr.Keywords("document-format-supported", "application/pdf", "application/octet-stream", "image/pwg-raster"),
		iattr.Keywords("print-color-mode-supported", "monochrome", "color"),
		iattr.Keywords("sides-supported", "one-sided", "two-sided-long-edge"),
		iattr.URI("printer-uuid", "urn:uuid:12345678-1234-1234-1234-123456789abc"),
		iattr.Text("printer-make-and-model", "Example Printer"),
	}
	txt := printerDNSSDTXT("printers/office", "Office", "Floor 2", attrs)
	if txt["rp"] != "printers/office" || txt["pdl"] != "application/pdf,image/pwg-raster" {
		t.Fatalf("unexpected routing/formats: %#v", txt)
	}
	if txt["Color"] != "T" || txt["Duplex"] != "T" || txt["ty"] != "Example Printer" {
		t.Fatalf("capability flags missing: %#v", txt)
	}
	if _, ok := txt["TLS"]; ok {
		t.Fatalf("plaintext TXT unexpectedly advertised TLS: %#v", txt)
	}
	if txt["UUID"] != "12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("UUID not normalized: %#v", txt)
	}
}

func TestDNSServiceInputUsesPublicPathAndPlaintextTXT(t *testing.T) {
	printer := config.PrinterConfig{
		DisplayName:       "Office",
		IPPEverywhereMode: config.IPPEverywhereAuto,
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://public.example:8631/printers"},
		DNSSD:  config.DNSSDConfig{Hostname: "proxy.local"},
		Printers: map[string]config.PrinterConfig{
			"office": printer,
		},
	}
	svc, err := NewService(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	input, err := svc.dnsServiceInput("office", printer, goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
			goipp.String("application/pdf"), goipp.String("application/octet-stream")),
		iattr.Keyword("ipp-features-supported", "ipp-everywhere"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.Hostname != "proxy.local" || input.Port != 8631 || input.IPPS {
		t.Fatalf("unexpected DNS-SD endpoint: %+v", input)
	}
	if input.TXT["rp"] != "printers/office" || input.TXT["pdl"] != "application/pdf" {
		t.Fatalf("DNS-SD TXT does not match client path/formats: %#v", input.TXT)
	}
	if _, ok := input.TXT["TLS"]; ok {
		t.Fatalf("plaintext DNS-SD input advertised TLS: %#v", input.TXT)
	}
}
