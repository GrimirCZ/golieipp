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

func TestNormalizeJobAttrsAllowsConfiguredMediaSelection(t *testing.T) {
	policy := config.PolicyConfig{
		MediaSupported: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		MediaDefault:   "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}
	out, log := NormalizeJobAttrs(goipp.Attributes{iattr.Keyword("media", "na_letter_8.5x11in")}, policy, nil)
	if !iattr.HasStringValue(out, "media", "na_letter_8.5x11in") {
		t.Fatal("configured client media was not preserved")
	}
	if log.EffectiveMedia != "na_letter_8.5x11in" || log.MediaFallback {
		t.Fatalf("unexpected selection log: %+v", log)
	}
}

func TestNormalizeJobAttrsFallsBackForUnsupportedOrMissingMedia(t *testing.T) {
	policy := config.PolicyConfig{
		MediaSupported: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		MediaDefault:   "na_letter_8.5x11in",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}
	for _, attrs := range []goipp.Attributes{
		{iattr.Keyword("media", "jis_b5_182x257mm")},
		nil,
	} {
		out, log := NormalizeJobAttrs(attrs, policy, nil)
		if !iattr.HasStringValue(out, "media", "na_letter_8.5x11in") || !log.MediaFallback {
			t.Fatalf("expected fallback for attrs=%v: out=%v log=%+v", attrs, out, log)
		}
	}
}

func TestNormalizeJobAttrsMediaColPrecedesMediaAndAcceptsReversedDimensions(t *testing.T) {
	policy := config.PolicyConfig{
		MediaSupported: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		MediaDefault:   "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
		UseMediaCol:    true,
	}
	mediaCol := goipp.MakeAttrCollection("media-col",
		goipp.MakeAttrCollection("media-size",
			iattr.Integer("x-dimension", 29700),
			iattr.Integer("y-dimension", 21000),
		),
	)
	attrs := goipp.Attributes{mediaCol, iattr.Keyword("media", "na_letter_8.5x11in")}
	out, log := NormalizeJobAttrs(attrs, policy, nil)
	if _, ok := iattr.Attr(out, "media"); ok {
		t.Fatal("media was emitted despite use_media_col=true")
	}
	if log.EffectiveMedia != "iso_a4_210x297mm" || log.MediaFallback {
		t.Fatalf("unexpected reversed-dimension selection: %+v", log)
	}
	col, ok := iattr.Attr(out, "media-col")
	if !ok || len(col.Values) != 1 {
		t.Fatalf("normalized media-col missing: %+v", out)
	}
	collection, ok := col.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("normalized media-col has wrong value: %#v", col.Values[0].V)
	}
	if _, ok := iattr.Attr(goipp.Attributes(collection), "media-key"); ok {
		t.Fatal("normalized media-col invented a printer-specific media-key")
	}
	if _, ok := iattr.Attr(goipp.Attributes(collection), "media-size-name"); ok {
		t.Fatal("normalized job media-col must not contain media-size-name")
	}
	mediaSizeAttr, ok := iattr.Attr(goipp.Attributes(collection), "media-size")
	if !ok || len(mediaSizeAttr.Values) != 1 {
		t.Fatalf("normalized media-size missing: %#v", collection)
	}
	mediaSize, ok := mediaSizeAttr.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("normalized media-size has wrong value: %#v", mediaSizeAttr.Values[0].V)
	}
	if x, _ := iattr.FirstInt(goipp.Attributes(mediaSize), "x-dimension"); x != 21000 {
		t.Fatalf("expected normalized A4 x dimension, got %d", x)
	}
}

func TestNormalizeJobAttrsMediaColUsesUpstreamCustomCatalog(t *testing.T) {
	policy := config.PolicyConfig{
		MediaSupported: []string{"custom_roll"},
		MediaDefault:   "custom_roll",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
		UseMediaCol:    true,
	}
	upstream := goipp.Attributes{
		mediaColAttr("media-col-database", mediaSize{Name: "custom_roll", XDimension: 10000, YDimension: 20000, HasDimension: true}, "stationery"),
		iattr.Keyword("media-supported", "custom_roll"),
	}
	request := goipp.Attributes{goipp.MakeAttrCollection("media-col", iattr.Keyword("media-key", "custom_roll"))}
	out, log := NormalizeJobAttrsWithCapabilities(request, policy, nil, upstream)
	if log.MediaFallback || log.EffectiveMedia != "custom_roll" {
		t.Fatalf("custom media was not resolved: %+v", log)
	}
	if _, ok := iattr.Attr(out, "media-col"); !ok {
		t.Fatal("normalized custom media-col missing")
	}
}

func TestNormalizeJobAttrsMediaColUsesOnlyJobSafeMembers(t *testing.T) {
	source := "tray-1"
	policy := config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "com.canon.oip.stationery-2",
		MediaSource:    &source,
		PrintColorMode: "monochrome",
		UseMediaCol:    true,
	}
	out, _ := NormalizeJobAttrs(nil, policy, nil)
	attr, ok := iattr.Attr(out, "media-col")
	if !ok || len(attr.Values) != 1 {
		t.Fatalf("normalized job media-col missing: %+v", out)
	}
	collection, ok := attr.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("normalized job media-col has wrong value: %#v", attr.Values[0].V)
	}
	members := goipp.Attributes(collection)
	for _, forbidden := range []string{"media-size-name", "media-key"} {
		if _, ok := iattr.Attr(members, forbidden); ok {
			t.Fatalf("job media-col contains printer-description member %q: %#v", forbidden, members)
		}
	}
	if got, _ := iattr.FirstString(members, "media-type"); got != policy.MediaType {
		t.Fatalf("forced media type missing: %q", got)
	}
	if got, _ := iattr.FirstString(members, "media-source"); got != source {
		t.Fatalf("forced media source missing: %q", got)
	}
	if countAttrs(out, "media-source") != 0 {
		t.Fatalf("use_media_col=true emitted a duplicate loose media-source: %#v", out)
	}
	mediaSizeAttr, ok := iattr.Attr(members, "media-size")
	if !ok || len(mediaSizeAttr.Values) != 1 {
		t.Fatalf("job media-size missing: %#v", members)
	}
	mediaSize, ok := mediaSizeAttr.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("job media-size has wrong value: %#v", mediaSizeAttr.Values[0].V)
	}
	if x, _ := iattr.FirstInt(goipp.Attributes(mediaSize), "x-dimension"); x != 21000 {
		t.Fatalf("job media-size x-dimension = %d", x)
	}
	if y, _ := iattr.FirstInt(goipp.Attributes(mediaSize), "y-dimension"); y != 29700 {
		t.Fatalf("job media-size y-dimension = %d", y)
	}
}

func TestNormalizeJobAttrsSanitizesPolicyAttrsInsideOverrides(t *testing.T) {
	policy := config.PolicyConfig{
		Media:          "iso_a4_210x297mm",
		MediaType:      "stationery",
		PrintColorMode: "monochrome",
	}
	overrides := goipp.MakeAttr("overrides", goipp.TagBeginCollection,
		goipp.Collection{
			iattr.Integer("document-number", 1),
			iattr.Keyword("media", "na_letter_8.5x11in"),
			goipp.MakeAttrCollection("media-col",
				goipp.MakeAttrCollection("media-size",
					iattr.Integer("x-dimension", 21590),
					iattr.Integer("y-dimension", 27940),
				),
			),
			iattr.Keyword("print-color-mode", "color"),
			iattr.Keyword("sides", "two-sided-long-edge"),
		},
		goipp.Collection{
			iattr.Integer("document-number", 2),
			iattr.Keyword("media", "na_letter_8.5x11in"),
		},
	)

	out, _ := NormalizeJobAttrs(goipp.Attributes{overrides}, policy, nil)
	if !iattr.HasStringValue(out, "media", "iso_a4_210x297mm") {
		t.Fatalf("top-level policy media missing: %#v", out)
	}
	attr, ok := iattr.Attr(out, "overrides")
	if !ok || len(attr.Values) != 1 {
		t.Fatalf("expected only the non-empty sanitized override collection, got %#v", attr)
	}
	collection, ok := attr.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("sanitized override has wrong value type: %#v", attr.Values[0].V)
	}
	members := goipp.Attributes(collection)
	for _, forbidden := range []string{"media", "media-col", "media-type", "media-source", "print-color-mode", "output-mode"} {
		if _, found := iattr.Attr(members, forbidden); found {
			t.Fatalf("policy-controlled override member %q bypassed normalization: %#v", forbidden, members)
		}
	}
	if _, ok := iattr.Attr(members, "media-size"); ok {
		t.Fatalf("nested media-col member bypassed normalization: %#v", members)
	}
	if !iattr.HasStringValue(members, "sides", "two-sided-long-edge") {
		t.Fatalf("non-policy override member was dropped: %#v", members)
	}
}

func countAttrs(attrs goipp.Attributes, name string) int {
	count := 0
	for _, attr := range attrs {
		if attr.Name == name {
			count++
		}
	}
	return count
}
