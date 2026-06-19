package proxy

import (
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestFilterPrinterAttributesRestrictsPolicyAndPreservesOtherCaps(t *testing.T) {
	upstream := goipp.Attributes{
		iattr.URI("printer-uri-supported", "ipp://real/ipp/print"),
		iattr.Keyword("media-supported", "na_letter_8.5x11in"),
		iattr.Keyword("print-color-mode-supported", "color"),
		iattr.Boolean("color-supported", true),
		iattr.Keywords("output-mode-supported", "auto", "monochrome", "color"),
		iattr.Keyword("output-mode-default", "auto"),
		goipp.MakeAttr("operations-supported", goipp.TagEnum,
			goipp.Integer(goipp.OpPrintJob),
			goipp.Integer(goipp.OpCreateJob),
			goipp.Integer(goipp.OpSendDocument),
		),
		iattr.Boolean("multiple-document-jobs-supported", true),
		iattr.Keywords("pwg-raster-document-type-supported", "srgb_8", "sgray_8", "rgb_8"),
		iattr.Keywords("urf-supported", "ADOBERGB24", "SRGB24", "W8-16"),
		iattr.Integer("pages-per-minute-color", 25),
		iattr.Keyword("sides-supported", "two-sided-long-edge"),
		goipp.MakeAttribute("document-format-supported", goipp.TagMimeType, goipp.String("application/pdf")),
	}

	out := FilterPrinterAttributes(upstream, "office", "ipp://proxy/printers/office", config.PrinterConfig{
		DisplayName: "Office A4 B&W",
		Location:    "Office",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	})

	if !iattr.HasStringValue(out, "media-supported", "iso_a4_210x297mm") {
		t.Fatal("filtered media missing")
	}
	if iattr.HasStringValue(out, "media-supported", "na_letter_8.5x11in") {
		t.Fatal("upstream media leaked")
	}
	if !iattr.HasStringValue(out, "print-color-mode-supported", "monochrome") {
		t.Fatal("filtered print-color-mode missing")
	}
	if _, ok := iattr.Attr(out, "printer-location"); !ok {
		t.Fatal("printer-location missing")
	}
	if !iattr.HasStringValue(out, "printer-location", "Office") {
		t.Fatal("configured printer-location missing")
	}
	if iattr.HasStringValue(out, "output-mode-supported", "color") {
		t.Fatal("upstream output-mode color leaked")
	}
	if !iattr.HasStringValue(out, "output-mode-supported", "monochrome") {
		t.Fatal("filtered output-mode missing")
	}
	if !hasEnumValue(out, "operations-supported", goipp.OpPrintJob) {
		t.Fatal("print-job operation missing")
	}
	if hasEnumValue(out, "operations-supported", goipp.OpCreateJob) {
		t.Fatal("create-job operation leaked")
	}
	if hasEnumValue(out, "operations-supported", goipp.OpSendDocument) {
		t.Fatal("send-document operation leaked")
	}
	if got, ok := iattr.FirstString(out, "multiple-document-jobs-supported"); !ok || got != "false" {
		t.Fatal("multiple-document-jobs-supported was not disabled")
	}
	if iattr.HasStringValue(out, "pwg-raster-document-type-supported", "srgb_8") {
		t.Fatal("upstream color raster type leaked")
	}
	if !iattr.HasStringValue(out, "pwg-raster-document-type-supported", "sgray_8") {
		t.Fatal("filtered grayscale raster type missing")
	}
	if iattr.HasStringValue(out, "urf-supported", "SRGB24") {
		t.Fatal("upstream color urf mode leaked")
	}
	if _, ok := iattr.Attr(out, "pages-per-minute-color"); ok {
		t.Fatal("color speed capability leaked")
	}
	if !iattr.HasStringValue(out, "sides-supported", "two-sided-long-edge") {
		t.Fatal("passthrough capability missing")
	}
	if !iattr.HasStringValue(out, "document-format-supported", "application/pdf") {
		t.Fatal("document format capability missing")
	}
}

func hasEnumValue(attrs goipp.Attributes, name string, value goipp.Op) bool {
	attr, ok := iattr.Attr(attrs, name)
	if !ok {
		return false
	}
	for _, val := range attr.Values {
		if enum, ok := val.V.(goipp.Integer); ok && goipp.Op(enum) == value {
			return true
		}
	}
	return false
}

func TestValidatePolicyAgainstUpstream(t *testing.T) {
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		iattr.Keyword("print-color-mode-supported", "monochrome"),
		iattr.Keyword("media-type-supported", "stationery"),
	}
	err := ValidatePolicyAgainstUpstream(upstream, config.PrinterConfig{
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}
