package proxy

import (
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestNormalizeJobAttrsDropsAndInjectsPolicy(t *testing.T) {
	scaling := "fit"
	attrs := goipp.Attributes{
		iattr.Keyword("media", "na_letter_8.5x11in"),
		iattr.Keyword("media-type", "stationery-heavyweight"),
		iattr.Keyword("print-color-mode", "color"),
		iattr.Keyword("output-mode", "color"),
		iattr.Keyword("ColorModel", "RGB"),
		iattr.Keyword("sides", "two-sided-long-edge"),
	}

	out, log := NormalizeJobAttrs(attrs, config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
		PrintScaling:   &scaling,
	}, []string{"ColorModel"})

	if _, ok := iattr.Attr(out, "ColorModel"); ok {
		t.Fatal("vendor color attribute was not dropped")
	}
	if !iattr.HasStringValue(out, "media", "iso_a4_210x297mm") {
		t.Fatal("forced media missing")
	}
	if !iattr.HasStringValue(out, "media-type", "stationery") {
		t.Fatal("forced media-type missing")
	}
	if !iattr.HasStringValue(out, "print-color-mode", "monochrome") {
		t.Fatal("forced color missing")
	}
	if !iattr.HasStringValue(out, "output-mode", "monochrome") {
		t.Fatal("forced output-mode missing")
	}
	if !iattr.HasStringValue(out, "sides", "two-sided-long-edge") {
		t.Fatal("non-policy job attribute was not preserved")
	}
	if log.ClientMedia != "na_letter_8.5x11in" || log.ClientPrintColorMode != "color" {
		t.Fatalf("unexpected normalization log: %+v", log)
	}
}

func TestNormalizeJobAttrsCanUseMediaCol(t *testing.T) {
	out, _ := NormalizeJobAttrs(nil, config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
		UseMediaCol:    true,
	}, nil)

	if _, ok := iattr.Attr(out, "media-col"); !ok {
		t.Fatal("media-col missing")
	}
	if _, ok := iattr.Attr(out, "media"); ok {
		t.Fatal("loose media should not be injected when use_media_col=true")
	}
}
