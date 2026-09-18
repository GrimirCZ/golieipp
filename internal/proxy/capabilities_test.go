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
	if _, ok := iattr.Attr(out, "overrides-supported"); ok {
		t.Fatalf("unsafe overrides-supported capability was advertised: %#v", out)
	}
	if _, ok := iattr.Attr(out, "media-source-supported"); ok {
		t.Fatal("nil media-source policy invented auto without upstream proof")
	}
	if got, ok := iattr.FirstString(out, "multiple-document-jobs-supported"); !ok || got != "false" {
		t.Fatal("multiple-document-jobs-supported was not disabled")
	}
	if iattr.HasStringValue(out, "pwg-raster-document-type-supported", "srgb_8") {
		t.Fatal("upstream color raster type leaked")
	}
	if _, ok := iattr.Attr(out, "pwg-raster-document-type-supported"); ok {
		t.Fatal("incomplete PWG Raster family was advertised")
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
	if a3.HasBottomMargin || a3.HasLeftMargin || a3.HasRightMargin || a3.HasTopMargin {
		t.Fatalf("unknown margins were invented from global supported values: %+v", a3)
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
	if size.HasBottomMargin || size.HasLeftMargin || size.HasRightMargin || size.HasTopMargin {
		t.Fatalf("malformed/ranged margins were not left unknown: %+v", size)
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

func TestBuildMediaCatalogKeepsDistinctInstancesAndUnknownMargins(t *testing.T) {
	policy := config.PolicyConfig{
		MediaSupported: []string{"iso_a4_210x297mm"},
		MediaDefault:   "iso_a4_210x297mm",
		MediaType:      "stationery",
	}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("media-col-database", goipp.TagBeginCollection,
			goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				iattr.Keyword("media-source", "tray-1"),
				iattr.Keyword("media-type", "stationery"),
				goipp.MakeAttrCollection("media-size", iattr.Integer("x-dimension", 21000), iattr.Integer("y-dimension", 29700)),
				iattr.Integer("media-bottom-margin", 0),
			}),
		goipp.MakeAttr("media-col-database", goipp.TagBeginCollection,
			goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				iattr.Keyword("media-source", "tray-2"),
				iattr.Keyword("media-type", "stationery"),
				goipp.MakeAttrCollection("media-size", iattr.Integer("x-dimension", 21000), iattr.Integer("y-dimension", 29700)),
				iattr.Integer("media-bottom-margin", 500),
				iattr.Integer("media-left-margin", 500),
				iattr.Integer("media-right-margin", 500),
				iattr.Integer("media-top-margin", 500),
			}),
	}

	catalog := buildMediaCatalog(upstream, policy)
	if len(catalog.Sizes) != 2 || len(catalog.Instances) != 2 {
		t.Fatalf("expected two complete media instances, got sizes=%+v instances=%+v", catalog.Sizes, catalog.Instances)
	}
	first, second := catalog.Instances[0], catalog.Instances[1]
	if first.MediaSource != "tray-1" || second.MediaSource != "tray-2" {
		t.Fatalf("media source identity was lost: %+v %+v", first, second)
	}
	if !first.HasBottomMargin || first.BottomMargin != 0 || first.HasLeftMargin {
		t.Fatalf("known zero and unknown margins were not distinguished: %+v", first)
	}
	if !second.HasLeftMargin || second.LeftMargin != 500 {
		t.Fatalf("non-zero margin was not retained: %+v", second)
	}
	if selected := catalog.ByName[mediaNameKey(policy.MediaDefault)]; selected.MediaSource != "tray-2" {
		t.Fatalf("ordinary default did not prefer the non-borderless instance: %+v", selected)
	}
}

func TestFilterPrinterAttributesSynthesizesColorAndCoupledRasterFamilies(t *testing.T) {
	printer := config.PrinterConfig{
		DisplayName: "Color",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "color",
		},
	}
	upstream := goipp.Attributes{
		iattr.Keywords("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
			goipp.String("application/pdf"), goipp.String("image/pwg-raster"), goipp.String("image/urf")),
		iattr.Keywords("pwg-raster-document-type-supported", "srgb_8", "sgray_8"),
		goipp.MakeAttribute("pwg-raster-document-resolution-supported", goipp.TagResolution,
			goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}),
		iattr.Keyword("pwg-raster-document-sheet-back", "normal"),
		iattr.Keywords("urf-supported", "W8-16", "SRGB24", "RS300"),
		iattr.Keywords("media-source-supported", "auto"),
	}
	out := FilterPrinterAttributes(upstream, "color", "ipp://proxy/printers/color", printer)
	if !iattr.HasStringValue(out, "print-color-mode-supported", "color") ||
		!iattr.HasStringValue(out, "output-mode-supported", "color") {
		t.Fatalf("configured color mode was not synthesized end-to-end: %+v", out)
	}
	if color, ok := iattr.FirstString(out, "color-supported"); !ok || color != "true" {
		t.Fatalf("color-supported did not match color policy: %q", color)
	}
	resolution, ok := iattr.Attr(out, "pwg-raster-document-resolution-supported")
	validResolution := ok && len(resolution.Values) == 1 && resolution.Values[0].T == goipp.TagResolution && resolution.Values[0].V == (goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi})
	if !iattr.HasStringValue(out, "pwg-raster-document-type-supported", "srgb_8") ||
		!validResolution || !iattr.HasStringValue(out, "pwg-raster-document-sheet-back", "normal") {
		t.Fatalf("complete PWG Raster family missing: %+v", out)
	}
	if iattr.HasStringValue(out, "pwg-raster-document-type-supported", "sgray_8") {
		t.Fatal("incompatible monochrome raster type leaked into color capabilities")
	}
	if _, ok := iattr.Attr(out, "urf-supported"); !ok {
		t.Fatal("complete URF capability was not retained")
	}
}

func TestFilterPrinterAttributesDoesNotAdvertiseIncompleteRasterOrURF(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
			goipp.String("application/pdf"), goipp.String("image/pwg-raster"), goipp.String("image/urf")),
		iattr.Keyword("pwg-raster-document-type-supported", "sgray_8"),
		iattr.Keywords("urf-supported", "W8-16", "SRGB24"), // missing RS resolution token
	}
	out := FilterPrinterAttributes(upstream, "mono", "ipp://proxy/printers/mono", printer)
	if _, ok := iattr.Attr(out, "pwg-raster-document-type-supported"); ok {
		t.Fatal("PWG Raster type was advertised without resolution and sheet-back")
	}
	if _, ok := iattr.Attr(out, "pwg-raster-document-resolution-supported"); ok {
		t.Fatal("PWG Raster resolution leaked without a complete family")
	}
	if _, ok := iattr.Attr(out, "urf-supported"); ok {
		t.Fatal("incomplete URF capability was advertised")
	}
	formats, _ := iattr.Attr(out, "document-format-supported")
	if hasString(formats, "image/pwg-raster") || hasString(formats, "image/urf") {
		t.Fatalf("incomplete raster formats leaked: %+v", formats)
	}
}

func TestFilterPrinterAttributesRejectsMalformedPWGRasterValueTags(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	base := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType, goipp.String("image/pwg-raster")),
		iattr.Keyword("pwg-raster-document-type-supported", "sgray_8"),
		goipp.MakeAttribute("pwg-raster-document-resolution-supported", goipp.TagResolution,
			goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}),
		iattr.Keyword("pwg-raster-document-sheet-back", "normal"),
	}

	t.Run("resolution requires resolution tag", func(t *testing.T) {
		upstream := append(goipp.Attributes{}, base...)
		upstream = iattr.SetAttr(upstream, goipp.MakeAttribute("pwg-raster-document-resolution-supported", goipp.TagInteger,
			goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}))
		out := FilterPrinterAttributes(upstream, "mono", "ipp://proxy/printers/mono", printer)
		if _, ok := iattr.Attr(out, "pwg-raster-document-resolution-supported"); ok {
			t.Fatalf("wrong-tag resolution was advertised: %+v", out)
		}
		formats, _ := iattr.Attr(out, "document-format-supported")
		if hasString(formats, "image/pwg-raster") {
			t.Fatalf("wrong-tag resolution kept image/pwg-raster: %+v", out)
		}
	})

	t.Run("sheet-back requires keyword tag", func(t *testing.T) {
		upstream := append(goipp.Attributes{}, base...)
		upstream = iattr.SetAttr(upstream, goipp.MakeAttribute("pwg-raster-document-sheet-back", goipp.TagName, goipp.String("normal")))
		out := FilterPrinterAttributes(upstream, "mono", "ipp://proxy/printers/mono", printer)
		if _, ok := iattr.Attr(out, "pwg-raster-document-sheet-back"); ok {
			t.Fatalf("wrong-tag sheet-back was advertised: %+v", out)
		}
		formats, _ := iattr.Attr(out, "document-format-supported")
		if hasString(formats, "image/pwg-raster") {
			t.Fatalf("wrong-tag sheet-back kept image/pwg-raster: %+v", out)
		}
	})
}

func TestFilterPrinterAttributesRequiresCompleteURFFamily(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	cases := []struct {
		name   string
		tokens []string
		valid  bool
	}{
		{
			name:   "minimal supported family",
			tokens: []string{"W8-16", "SRGB24", "RS300"},
			valid:  true,
		},
		{
			name:   "complete AirPrint family",
			tokens: []string{"V1.4", "W8", "SRGB24", "CP255", "FN3-11", "IS9", "IFU0", "MT1-2", "OB10", "PQ3-4-5", "RS300-600"},
			valid:  true,
		},
		{
			name:   "missing bit depth",
			tokens: []string{"SRGB24", "RS300"},
		},
		{
			name:   "missing color space",
			tokens: []string{"W8", "RS300"},
		},
		{
			name:   "missing resolution",
			tokens: []string{"W8", "SRGB24"},
		},
		{
			name:   "zero resolution",
			tokens: []string{"W8", "SRGB24", "RS0"},
		},
		{
			name:   "zero in resolution list",
			tokens: []string{"W8", "SRGB24", "RS300-0"},
		},
		{
			name:   "zero width",
			tokens: []string{"W0", "SRGB24", "RS300"},
		},
		{
			name:   "zero color depth",
			tokens: []string{"W8", "SRGB0", "RS300"},
		},
		{
			name:   "zero required numeric value",
			tokens: []string{"W8", "SRGB24", "CP0", "RS300"},
		},
		{
			name:   "zero required numeric list member",
			tokens: []string{"W8", "SRGB24", "FN3-0", "RS300"},
		},
		{
			name:   "zero optional image format values are valid",
			tokens: []string{"W8", "SRGB24", "IFU0", "OFU0", "RS300"},
			valid:  true,
		},
		{
			name:   "unknown token",
			tokens: []string{"W8", "SRGB24", "RS300", "not-urf"},
		},
		{
			name:   "malformed numeric token",
			tokens: []string{"W8-", "SRGB24", "RS300"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := goipp.Attributes{
				iattr.Keyword("media-supported", "iso_a4_210x297mm"),
				goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
					goipp.String("application/pdf"), goipp.String("image/urf")),
				iattr.Keywords("urf-supported", tc.tokens...),
			}
			out := FilterPrinterAttributes(upstream, "urf", "ipp://proxy/printers/urf", printer)
			_, advertised := iattr.Attr(out, "urf-supported")
			formats, _ := iattr.Attr(out, "document-format-supported")
			urfFormat := hasString(formats, "image/urf")
			if advertised != tc.valid || urfFormat != tc.valid {
				t.Fatalf("URF family validity=%t, advertised=%t, image/urf=%t, output=%+v", tc.valid, advertised, urfFormat, out)
			}
		})
	}
}

func TestFilterPrinterAttributesMediaSourceNilUsesOnlyProvenUpstreamSource(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	base := goipp.Attributes{iattr.Keyword("media-supported", "iso_a4_210x297mm")}
	out := FilterPrinterAttributes(base, "office", "ipp://proxy/printers/office", printer)
	if _, ok := iattr.Attr(out, "media-source-supported"); ok {
		t.Fatal("media source was invented without upstream proof")
	}
	out = FilterPrinterAttributes(append(base, iattr.Keywords("media-source-supported", "tray-1", "auto")), "office", "ipp://proxy/printers/office", printer)
	if !iattr.HasStringValue(out, "media-source-supported", "auto") || !iattr.HasStringValue(out, "media-source-default", "auto") {
		t.Fatalf("proven auto source was not retained: %+v", out)
	}
}

func TestCapabilityModelReportsDynamicIPPFeatureEligibility(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	claimed := goipp.Attributes{iattr.Keyword("ipp-features-supported", "ipp-everywhere")}
	model := NewCapabilityModel(claimed, "office", "ipp://proxy/printers/office", printer, CapabilityModelOptions{
		Operations: []goipp.Op{goipp.OpGetPrinterAttributes},
	})
	if !model.IPPEligible || len(model.IPPFeatures) != 2 {
		t.Fatalf("trusted upstream claim was not advertised: %+v", model)
	}
	if len(model.RequiredUnsatisfied) == 0 {
		t.Fatal("missing required operations were not represented")
	}
	if _, ok := iattr.Attr(model.Attributes, "ipp-features-supported"); !ok {
		t.Fatal("trusted upstream claim was omitted because of local diagnostic gaps")
	}

	model = NewCapabilityModel(claimed, "office", "ipp://proxy/printers/office", printer, CapabilityModelOptions{Disabled: true})
	if model.IPPEligible || len(model.IPPFeatures) != 0 {
		t.Fatalf("disabled model was eligible: %+v", model)
	}
	if _, ok := iattr.Attr(model.Attributes, "ipp-features-supported"); ok {
		t.Fatal("disabled model advertised IPP features")
	}

	model = NewCapabilityModel(claimed, "office", "ipp://proxy/printers/office", printer, CapabilityModelOptions{})
	if !model.IPPEligible || len(model.IPPFeatures) != 2 {
		t.Fatalf("complete default operation set was not eligible: %+v", model)
	}
	features, _ := iattr.Attr(model.Attributes, "ipp-features-supported")
	if !hasString(features, "ipp-everywhere") || !hasString(features, "ipp-everywhere-server") {
		t.Fatalf("eligible model omitted required features: %+v", features)
	}
}

func TestPracticalCapabilityProfilesAreIndependentAndDoNotMirrorOctetStream(t *testing.T) {
	printer := config.PrinterConfig{
		AirPrintMode:      config.AirPrintAuto,
		IPPEverywhereMode: config.IPPEverywhereAuto,
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("operations-supported", goipp.TagEnum,
			goipp.Integer(goipp.OpGetPrinterAttributes),
			goipp.Integer(goipp.OpPrintJob),
			goipp.Integer(goipp.OpValidateJob),
			goipp.Integer(goipp.OpCreateJob),
			goipp.Integer(goipp.OpSendDocument),
			goipp.Integer(goipp.OpCancelJob),
			goipp.Integer(goipp.OpGetJobAttributes),
			goipp.Integer(goipp.OpGetJobs),
			goipp.Integer(goipp.OpCancelMyJobs),
			goipp.Integer(goipp.OpCloseJob),
			goipp.Integer(goipp.OpIdentifyPrinter)),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
			goipp.String("application/pdf"), goipp.String("application/octet-stream"), goipp.String("image/urf")),
		iattr.Keyword("ipp-features-supported", "ipp-everywhere"),
		iattr.Keywords("urf-supported", "V1.4", "W8", "SRGB24", "RS300"),
	}
	model := NewCapabilityModel(upstream, "office", "ipp://proxy/printers/office", printer, CapabilityModelOptions{
		Operations:        proxySupportedOperations(upstream),
		PracticalProfiles: true,
	}, defaultPrinterIdentityMetadata("ipp://proxy/printers/office"))
	if !model.Profiles.Ordinary.Ready || !model.Profiles.AirPrint.Ready || !model.Profiles.IPPEverywhere.Ready {
		t.Fatalf("complete practical capability set was not ready: %+v", model.Profiles)
	}
	if !model.IPPEligible || !iattr.HasStringValue(model.Attributes, "ipp-features-supported", "ipp-everywhere") {
		t.Fatalf("ready practical IPP Everywhere profile was not advertised: %+v", model)
	}
	if iattr.HasStringValue(model.Attributes, "document-format-supported", "application/octet-stream") {
		t.Fatal("application/octet-stream leaked into the client format capability")
	}
	if got, ok := iattr.FirstString(model.Attributes, "print-color-mode-supported"); !ok || got != "monochrome" {
		t.Fatalf("monochrome policy was not synthesized: %q", got)
	}

	printer.AirPrintMode = config.AirPrintDisabled
	disabledAirPrint := NewCapabilityModel(upstream, "office", "ipp://proxy/printers/office", printer, CapabilityModelOptions{
		Operations:        proxySupportedOperations(upstream),
		PracticalProfiles: true,
	}, defaultPrinterIdentityMetadata("ipp://proxy/printers/office"))
	if !disabledAirPrint.Profiles.Ordinary.Ready || !disabledAirPrint.Profiles.IPPEverywhere.Ready {
		t.Fatalf("disabling AirPrint changed unrelated profile readiness: %+v", disabledAirPrint.Profiles)
	}
	if disabledAirPrint.Profiles.AirPrint.Ready || disabledAirPrint.Profiles.AirPrint.Reason == "" {
		t.Fatalf("AirPrint disable was not isolated: %+v", disabledAirPrint.Profiles.AirPrint)
	}
}

func TestCapabilityModelHonorsPrinterIPPEverywhereDisabledMode(t *testing.T) {
	printer := config.PrinterConfig{IPPEverywhereMode: config.IPPEverywhereDisabled, Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	model := NewCapabilityModel(
		goipp.Attributes{iattr.Keyword("ipp-features-supported", "ipp-everywhere")},
		"office", "ipp://proxy/printers/office", printer, CapabilityModelOptions{},
	)
	if !model.Disabled {
		t.Fatalf("printer disabled mode was ignored: %+v", model)
	}
	if model.IPPEligible || len(model.IPPFeatures) != 0 {
		t.Fatalf("disabled printer was advertised as IPP Everywhere: %+v", model)
	}
	if _, ok := iattr.Attr(model.Attributes, "ipp-features-supported"); ok {
		t.Fatalf("disabled printer leaked IPP features: %+v", model.Attributes)
	}
}

func TestCapabilityModelExplicitEmptyOperationsNeverEmitsEmptyAttribute(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}}
	model := NewCapabilityModel(
		goipp.Attributes{iattr.Keyword("ipp-features-supported", "ipp-everywhere")},
		"office", "ipp://proxy/printers/office", printer,
		CapabilityModelOptions{Operations: []goipp.Op{}},
	)
	if !model.Disabled {
		t.Fatalf("empty operation surface did not deactivate model: %+v", model)
	}
	if model.IPPEligible || len(model.IPPFeatures) != 0 {
		t.Fatalf("empty operation surface advertised IPP Everywhere: %+v", model)
	}
	attr, ok := iattr.Attr(model.Attributes, "operations-supported")
	if !ok || len(attr.Values) == 0 {
		t.Fatalf("operations-supported was empty or missing: %+v", model.Attributes)
	}
}

func TestFilterPrinterAttributesAcceptsSpecValidPWGRasterTypes(t *testing.T) {
	printer := config.PrinterConfig{Policy: config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "color",
	}}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType, goipp.String("image/pwg-raster")),
		iattr.Keywords("pwg-raster-document-type-supported", "cmyk_8", "device1_8", "srgb_8", "sgray_8"),
		goipp.MakeAttribute("pwg-raster-document-resolution-supported", goipp.TagResolution,
			goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}),
		iattr.Keyword("pwg-raster-document-sheet-back", "manual-tumble"),
	}
	out := FilterPrinterAttributes(upstream, "color", "ipp://proxy/printers/color", printer)
	for _, rasterType := range []string{"cmyk_8", "device1_8", "srgb_8"} {
		if !iattr.HasStringValue(out, "pwg-raster-document-type-supported", rasterType) {
			t.Fatalf("valid color PWG Raster type %q was filtered: %+v", rasterType, out)
		}
	}
	if iattr.HasStringValue(out, "pwg-raster-document-type-supported", "sgray_8") {
		t.Fatal("monochrome PWG Raster type leaked into color capabilities")
	}
	if !iattr.HasStringValue(out, "pwg-raster-document-sheet-back", "manual-tumble") {
		t.Fatalf("manual-tumble sheet-back was filtered: %+v", out)
	}
}

func TestMediaSizeFromPWGNameRejectsUnsafeRoundedDimensions(t *testing.T) {
	for _, name := range []string{
		"tiny_0.001x1mm",
		"huge_9223372036854775808x1mm",
		"huge_100000000000000000000x1in",
	} {
		if _, ok := mediaSizeFromPWGName(name); ok {
			t.Fatalf("unsafe media dimensions were accepted: %q", name)
		}
	}
	parsed, ok := mediaSizeFromPWGName("safe_0.01x1mm")
	if !ok || parsed.XDimension != 1 || parsed.YDimension != 100 {
		t.Fatalf("valid rounded media dimensions were rejected or changed: %+v, ok=%t", parsed, ok)
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
