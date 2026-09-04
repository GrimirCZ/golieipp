package proxy

import (
	"strings"
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
		iattr.Keywords("overrides-supported", "media", "media-col", "document-numbers", "pages"),
		iattr.Boolean("multiple-document-jobs-supported", true),
		iattr.Keywords("pwg-raster-document-type-supported", "srgb_8", "sgray_8", "rgb_8"),
		iattr.Keywords("urf-supported", "ADOBERGB24", "SRGB24", "W8-16"),
		goipp.MakeAttr("media-size-supported", goipp.TagBeginCollection,
			goipp.Collection{
				iattr.Integer("x-dimension", 21590),
				iattr.Integer("y-dimension", 27940),
			}),
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
	mediaSizes, ok := iattr.Attr(out, "media-size-supported")
	if !ok || len(mediaSizes.Values) != 1 {
		t.Fatalf("expected one allowlisted media-size-supported collection, got %+v", mediaSizes)
	}
	collection, ok := mediaSizes.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("media-size-supported has wrong value: %#v", mediaSizes.Values[0].V)
	}
	if x, _ := iattr.FirstInt(goipp.Attributes(collection), "x-dimension"); x != 21000 {
		t.Fatalf("unallowlisted media-size-supported leaked, x=%d", x)
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
	for _, op := range []goipp.Op{
		goipp.OpPrintJob,
		goipp.OpValidateJob,
		goipp.OpCreateJob,
		goipp.OpSendDocument,
		goipp.OpCancelJob,
		goipp.OpGetJobAttributes,
		goipp.OpGetJobs,
		goipp.OpGetPrinterAttributes,
		goipp.OpCancelMyJobs,
		goipp.OpCloseJob,
		goipp.OpIdentifyPrinter,
	} {
		if !hasEnumValue(out, "operations-supported", op) {
			t.Fatalf("%s operation missing", op)
		}
	}
	if _, ok := iattr.Attr(out, "overrides-supported"); !ok {
		t.Fatal("overrides-supported missing")
	}
	if !iattr.HasStringValue(out, "overrides-supported", "document-number") || !iattr.HasStringValue(out, "overrides-supported", "pages") {
		t.Fatalf("standards-required override targets missing: %#v", out)
	}
	if iattr.HasStringValue(out, "overrides-supported", "document-numbers") || iattr.HasStringValue(out, "overrides-supported", "media") || iattr.HasStringValue(out, "overrides-supported", "media-col") {
		t.Fatalf("upstream override targets leaked: %#v", out)
	}
	mediaSource, ok := iattr.Attr(out, "media-source-supported")
	if !ok || len(mediaSource.Values) != 1 || mediaSource.Values[0].T != goipp.TagKeyword || !iattr.HasStringValue(out, "media-source-supported", "auto") {
		t.Fatalf("nil media-source policy did not advertise keyword auto: %#v", mediaSource)
	}
	if !iattr.HasStringValue(out, "media-source-default", "auto") {
		t.Fatal("nil media-source policy did not advertise auto default")
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
	if _, ok := iattr.Attr(out, "urf-supported"); ok {
		t.Fatal("URF capability was advertised without image/urf document-format support")
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

func TestFilterPrinterAttributesAdvertisesEveryAllowedMediaSize(t *testing.T) {
	upstream := goipp.Attributes{
		iattr.Keywords("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in", "jis_b5_182x257mm"),
		iattr.Keyword("media-type-supported", "stationery"),
	}
	printer := config.PrinterConfig{
		DisplayName: "Office",
		Policy: config.PolicyConfig{
			MediaSupported: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
			MediaDefault:   "na_letter_8.5x11in",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	out := FilterPrinterAttributes(upstream, "office", "ipp://proxy/printers/office", printer)
	media, ok := iattr.Attr(out, "media-supported")
	if !ok || len(media.Values) != 2 || !hasString(media, "iso_a4_210x297mm") || !hasString(media, "na_letter_8.5x11in") {
		t.Fatalf("unexpected filtered media: %+v", media)
	}
	if !iattr.HasStringValue(out, "media-default", "na_letter_8.5x11in") {
		t.Fatal("configured media default missing")
	}
	if countAttributes(out, "media-default") != 1 {
		t.Fatalf("media-default must be emitted exactly once, got %d", countAttributes(out, "media-default"))
	}
	if iattr.HasStringValue(out, "media-col-supported", "media-key") || !iattr.HasStringValue(out, "media-col-supported", "media-size-name") {
		t.Fatal("media-col-supported advertised an inconsistent identifier")
	}
	database, ok := iattr.Attr(out, "media-col-database")
	if !ok || len(database.Values) != 2 {
		t.Fatalf("expected two media-col-database collections, got %+v", database)
	}
	if collection, ok := database.Values[0].V.(goipp.Collection); ok {
		if _, hasMediaKey := iattr.Attr(goipp.Attributes(collection), "media-key"); hasMediaKey {
			t.Fatal("media-col-database invented a printer-specific media-key")
		}
	}
	if _, ok := iattr.Attr(out, "media-col-default"); !ok {
		t.Fatal("media-col-default missing")
	}
	if iattr.HasStringValue(out, "media-supported", "jis_b5_182x257mm") {
		t.Fatal("upstream-only media leaked into filtered capabilities")
	}
}

func TestValidatePolicyAgainstUpstreamRequiresAllConfiguredMedia(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		MediaSupported: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		MediaDefault:   "iso_a4_210x297mm",
		PrintColorMode: "monochrome",
	}}
	err := ValidatePolicyAgainstUpstream(goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
	}, printer)
	if err == nil || !strings.Contains(err.Error(), "na_letter_8.5x11in") {
		t.Fatalf("expected missing media error, got %v", err)
	}
}

func TestValidatePolicyAgainstUpstreamResolvesMediaColDimensions(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		MediaSupported: []string{"custom_a4"},
		MediaDefault:   "custom_a4",
		PrintColorMode: "monochrome",
		UseMediaCol:    true,
	}}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "custom_a4"),
		mediaColAttr("media-col-database", mediaSize{Name: "custom_a4", XDimension: 21000, YDimension: 29700, HasDimension: true}, ""),
	}
	if err := ValidatePolicyAgainstUpstream(upstream, printer); err != nil {
		t.Fatal(err)
	}
	upstream = goipp.Attributes{upstream[0]}
	if err := ValidatePolicyAgainstUpstream(upstream, printer); err == nil || !strings.Contains(err.Error(), "fixed dimensions") {
		t.Fatalf("expected unresolved dimension error, got %v", err)
	}
}

func TestFilterPrinterAttributesBuildsCompleteCanonStyleMediaDatabase(t *testing.T) {
	printer := config.PrinterConfig{
		DisplayName: "IPP Dílny",
		Policy: config.PolicyConfig{
			MediaSupported: []string{"iso_a4_210x297mm", "iso_a3_297x420mm"},
			MediaDefault:   "iso_a4_210x297mm",
			MediaType:      "com.canon.oip.stationery-2",
			PrintColorMode: "monochrome",
		},
	}
	upstream := canonMediaAttributes()
	out := FilterPrinterAttributes(upstream, "dilny", "ipp://proxy/printers/dilny", printer)

	supported, ok := iattr.Attr(out, "media-col-supported")
	if !ok {
		t.Fatal("media-col-supported missing")
	}
	for _, member := range []string{
		"media-size-name", "media-size", "media-type",
		"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin",
	} {
		if !hasString(supported, member) {
			t.Fatalf("media-col-supported missing emitted member %q: %v", member, supported)
		}
	}

	database, ok := iattr.Attr(out, "media-col-database")
	if !ok || len(database.Values) != 2 {
		t.Fatalf("expected complete A4/A3 database, got %+v", database)
	}
	for _, value := range database.Values {
		collection, ok := value.V.(goipp.Collection)
		if !ok {
			t.Fatalf("database value is not a collection: %#v", value.V)
		}
		members := goipp.Attributes(collection)
		if _, ok := iattr.Attr(members, "media-key"); ok {
			t.Fatal("upstream Canon media-key leaked into filtered database")
		}
		for _, member := range []string{
			"media-size-name", "media-size", "media-type",
			"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin",
		} {
			attr, ok := iattr.Attr(members, member)
			if !ok || len(attr.Values) != 1 || attr.Values[0].T != goipp.TagInteger && member != "media-size-name" && member != "media-type" && member != "media-size" {
				t.Fatalf("database collection missing fixed member %q: %#v", member, members)
			}
		}
		mediaSizeAttr, _ := iattr.Attr(members, "media-size")
		mediaSize, ok := mediaSizeAttr.Values[0].V.(goipp.Collection)
		if !ok {
			t.Fatalf("media-size is not a collection: %#v", mediaSizeAttr)
		}
		if x, _ := iattr.FirstInt(goipp.Attributes(mediaSize), "x-dimension"); x <= 0 {
			t.Fatalf("invalid x-dimension: %d", x)
		}
		if y, _ := iattr.FirstInt(goipp.Attributes(mediaSize), "y-dimension"); y <= 0 {
			t.Fatalf("invalid y-dimension: %d", y)
		}
		if got, _ := iattr.FirstString(members, "media-type"); got != printer.Policy.MediaType {
			t.Fatalf("configured media type was not preserved: %q", got)
		}
	}

	ready, ok := iattr.Attr(out, "media-ready")
	if !ok || len(ready.Values) != 1 || !hasString(ready, "iso_a4_210x297mm") {
		t.Fatalf("unexpected filtered media-ready: %+v", ready)
	}
	if hasString(ready, "iso_a3_297x420mm") || hasString(ready, "na_letter_8.5x11in") {
		t.Fatalf("media-ready claimed a non-ready/disallowed size: %+v", ready)
	}
	readyCols, ok := iattr.Attr(out, "media-col-ready")
	if !ok || len(readyCols.Values) != 1 {
		t.Fatalf("expected only ready A4 collection, got %+v", readyCols)
	}
	readyCollection, _ := readyCols.Values[0].V.(goipp.Collection)
	if !iattr.HasStringValue(goipp.Attributes(readyCollection), "media-size-name", "iso_a4_210x297mm") {
		t.Fatalf("ready collection did not resolve to configured A4: %#v", readyCollection)
	}
}

func TestBuildMediaCatalogUsesMinimumSupportedMarginsForMissingCollection(t *testing.T) {
	policy := config.PolicyConfig{
		MediaSupported: []string{"iso_a4_210x297mm", "iso_a3_297x420mm"},
		MediaDefault:   "iso_a4_210x297mm",
		MediaType:      "stationery",
	}
	upstream := goipp.Attributes{
		iattr.Keywords("media-supported", "iso_a4_210x297mm", "iso_a3_297x420mm"),
		goipp.MakeAttr("media-col-database", goipp.TagBeginCollection,
			goipp.Collection{
				iattr.Keyword("media-key", "canon-a4"),
				goipp.MakeAttrCollection("media-size", iattr.Integer("x-dimension", 21000), iattr.Integer("y-dimension", 29700)),
			}),
		goipp.MakeAttr("media-bottom-margin-supported", goipp.TagInteger, goipp.Integer(500), goipp.Integer(0)),
		goipp.MakeAttr("media-left-margin-supported", goipp.TagInteger, goipp.Integer(500), goipp.Integer(100)),
		goipp.MakeAttr("media-right-margin-supported", goipp.TagInteger, goipp.Integer(250), goipp.Integer(500)),
		goipp.MakeAttr("media-top-margin-supported", goipp.TagInteger, goipp.Integer(500), goipp.Integer(50)),
	}
	catalog := buildMediaCatalog(upstream, policy)
	if len(catalog.Sizes) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	a3 := catalog.Sizes[1]
	if a3.BottomMargin != 0 || a3.LeftMargin != 100 || a3.RightMargin != 250 || a3.TopMargin != 50 {
		t.Fatalf("minimum supported margins were not selected: %+v", a3)
	}
	for _, size := range catalog.Sizes {
		if !size.HasDimension {
			t.Fatalf("catalog size has no fixed dimensions: %+v", size)
		}
	}
}

func TestBuildMediaCatalogIgnoresMalformedOrRangedMargins(t *testing.T) {
	policy := config.PolicyConfig{Media: "iso_a4_210x297mm"}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("media-col-database", goipp.TagBeginCollection,
			goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				goipp.MakeAttrCollection("media-size", iattr.Integer("x-dimension", 21000), iattr.Integer("y-dimension", 29700)),
				goipp.MakeAttr("media-bottom-margin", goipp.TagRange, goipp.Range{Lower: 0, Upper: 500}),
				goipp.MakeAttr("media-left-margin", goipp.TagInteger, goipp.Integer(500), goipp.Integer(100)),
				iattr.Integer("media-right-margin", -10),
			}),
		goipp.MakeAttr("media-bottom-margin-supported", goipp.TagRange, goipp.Range{Lower: 0, Upper: 500}),
		goipp.MakeAttr("media-left-margin-supported", goipp.TagInteger, goipp.Integer(200)),
	}
	catalog := buildMediaCatalog(upstream, policy)
	size := catalog.Sizes[0]
	if size.BottomMargin != 0 || size.LeftMargin != 200 || size.RightMargin != 0 || size.TopMargin != 0 {
		t.Fatalf("malformed/ranged margin handling was not deterministic: %+v", size)
	}
}

func TestValidatePolicyRequiresExactMediaForLooseMode(t *testing.T) {
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "canon-a4"),
		mediaColAttr("media-col-database", mediaSize{
			Name: "canon-a4", XDimension: 21000, YDimension: 29700, HasDimension: true,
		}, ""),
	}
	loose := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		PrintColorMode: "monochrome",
	}}
	if err := ValidatePolicyAgainstUpstream(upstream, loose); err == nil {
		t.Fatal("loose media mode accepted a name absent from media-supported")
	}
	col := loose
	col.Policy.UseMediaCol = true
	if err := ValidatePolicyAgainstUpstream(upstream, col); err != nil {
		t.Fatalf("media-col mode did not use collection/dimension fallback: %v", err)
	}
}

func countAttributes(attrs goipp.Attributes, name string) int {
	count := 0
	for _, attr := range attrs {
		if strings.EqualFold(attr.Name, name) {
			count++
		}
	}
	return count
}

func canonMediaAttributes() goipp.Attributes {
	mediaCollection := func(key string, x, y int, margins ...int) goipp.Collection {
		members := goipp.Attributes{
			iattr.Keyword("media-key", key),
			goipp.MakeAttrCollection("media-size", iattr.Integer("x-dimension", x), iattr.Integer("y-dimension", y)),
		}
		for index, name := range []string{"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin"} {
			members = append(members, iattr.Integer(name, margins[index]))
		}
		return goipp.Collection(members)
	}
	return goipp.Attributes{
		iattr.Keywords("media-supported", "canon_a4", "canon_a3", "na_letter_8.5x11in"),
		goipp.MakeAttr("media-col-database", goipp.TagBeginCollection,
			mediaCollection("canon_a4", 21000, 29700, 500, 0, 500, 0),
			mediaCollection("canon_a3", 29700, 42000, 500, 0, 500, 0)),
		iattr.Keywords("media-ready", "ISO_A4_210X297MM", "na_letter_8.5x11in"),
		goipp.MakeAttr("media-col-ready", goipp.TagBeginCollection,
			mediaCollection("canon_a4", 21000, 29700, 500, 0, 500, 0)),
		goipp.MakeAttr("media-bottom-margin-supported", goipp.TagInteger, goipp.Integer(0), goipp.Integer(500)),
		goipp.MakeAttr("media-left-margin-supported", goipp.TagInteger, goipp.Integer(0), goipp.Integer(500)),
		goipp.MakeAttr("media-right-margin-supported", goipp.TagInteger, goipp.Integer(0), goipp.Integer(500)),
		goipp.MakeAttr("media-top-margin-supported", goipp.TagInteger, goipp.Integer(0), goipp.Integer(500)),
	}
}
