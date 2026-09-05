package proxy

import (
	"testing"

	"github.com/OpenPrinting/goipp"
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
	txt := printerDNSSDTXT("printers/office", "Office", "Floor 2", attrs, true)
	if txt["rp"] != "printers/office" || txt["pdl"] != "application/pdf,image/pwg-raster" {
		t.Fatalf("unexpected routing/formats: %#v", txt)
	}
	if txt["Color"] != "T" || txt["Duplex"] != "T" || txt["TLS"] == "" {
		t.Fatalf("capability flags missing: %#v", txt)
	}
	if txt["UUID"] != "12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("UUID not normalized: %#v", txt)
	}
}
