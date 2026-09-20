package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/urf"
)

// AirPrintPath is the path exposed by a queue's client-facing capability
// snapshot.  It is deliberately a small value set because the same value is
// used by logs, diagnostics, and DNS-SD readiness.
type AirPrintPath string

const (
	AirPrintPathNative      AirPrintPath = "native"
	AirPrintPathEmulated    AirPrintPath = "emulated"
	AirPrintPathUnavailable AirPrintPath = "unavailable"
)

// DocumentRoute is an immutable client-to-upstream document route.  A route
// is selected from one committed capability snapshot and must be used for the
// complete lifetime of a multi-operation job.
type DocumentRoute struct {
	ClientFormat   string
	UpstreamFormat string
	Transform      bool
	Mapping        urf.Mapping
	Page           urf.PageSettings
	Resolutions    []uint32
	URFTokens      []string
}

func (r DocumentRoute) clone() DocumentRoute {
	r.Resolutions = append([]uint32(nil), r.Resolutions...)
	r.URFTokens = append([]string(nil), r.URFTokens...)
	return r
}

func (r DocumentRoute) matches(format string) bool {
	return strings.EqualFold(strings.TrimSpace(r.ClientFormat), strings.TrimSpace(format))
}

func (r DocumentRoute) String() string {
	if !r.Transform {
		return fmt.Sprintf("%s->%s", r.ClientFormat, r.UpstreamFormat)
	}
	return fmt.Sprintf("%s->%s (%s)", r.ClientFormat, r.UpstreamFormat, r.Mapping)
}

// RouteSnapshot is part of CapabilityModel and is copied with the capability
// snapshot.  Routes are intentionally derived only from validated upstream
// capabilities and effective queue policy.
type RouteSnapshot struct {
	Routes       []DocumentRoute
	AirPrintPath AirPrintPath
	Reason       string
	URFTokens    []string
}

func (s RouteSnapshot) clone() RouteSnapshot {
	copySnapshot := s
	copySnapshot.Routes = make([]DocumentRoute, len(s.Routes))
	for i, route := range s.Routes {
		copySnapshot.Routes[i] = route.clone()
	}
	copySnapshot.URFTokens = append([]string(nil), s.URFTokens...)
	return copySnapshot
}

func (s RouteSnapshot) routeFor(format string) (DocumentRoute, bool) {
	for _, route := range s.Routes {
		if route.matches(format) {
			return route.clone(), true
		}
	}
	return DocumentRoute{}, false
}

func (s RouteSnapshot) defaultRoute(upstream goipp.Attributes) (DocumentRoute, bool) {
	if format, ok := iattr.FirstString(upstream, "document-format-default"); ok {
		if route, found := s.routeFor(format); found {
			return route, true
		}
	}
	if s.AirPrintPath == AirPrintPathEmulated {
		if route, found := s.routeFor("image/urf"); found {
			return route, true
		}
	}
	if len(s.Routes) > 0 {
		return s.Routes[0].clone(), true
	}
	return DocumentRoute{}, false
}

func routeSnapshotForModel(model CapabilityModel) RouteSnapshot {
	if len(model.Routes.Routes) != 0 || model.Routes.AirPrintPath != "" {
		return model.Routes.clone()
	}
	return buildRouteSnapshot(model.Upstream, model.Policy, model.Printer)
}

func buildRouteSnapshot(upstream goipp.Attributes, policy config.PolicyConfig, printer config.PrinterConfig) RouteSnapshot {
	snapshot := RouteSnapshot{AirPrintPath: AirPrintPathUnavailable, Reason: "upstream does not provide an admitted AirPrint document route"}
	formats := admittedUpstreamFormats(upstream)
	nativeURF := validURFFamily(upstream)
	var nativeTokens []string
	if nativeURF {
		var policyCompatible bool
		nativeTokens, policyCompatible = nativeURFTokens(upstream, policy)
		nativeURF = policyCompatible
		if !policyCompatible {
			snapshot.Reason = "native URF family has no policy-compatible color-space mapping"
		}
	}
	if nativeURF {
		snapshot.AirPrintPath = AirPrintPathNative
		snapshot.Reason = "upstream provides a valid native image/urf family"
	}
	for _, format := range formats {
		if strings.EqualFold(format, "image/urf") && !nativeURF {
			continue
		}
		if strings.EqualFold(format, "image/pwg-raster") && !validPWGRasterFamily(upstream, policy) {
			continue
		}
		snapshot.Routes = append(snapshot.Routes, DocumentRoute{
			ClientFormat: format, UpstreamFormat: format,
			Page: defaultPageSettings(upstream, policy),
		})
	}
	if nativeURF {
		snapshot.URFTokens = nativeTokens
		// A native route always wins. Preserve the native payload unchanged.
		return snapshot
	}
	if normalizeAirPrintMode(printer.AirPrintMode) != config.AirPrintEmulateIfMissing {
		if normalizeAirPrintMode(printer.AirPrintMode) == config.AirPrintDisabled {
			snapshot.Reason = "disabled by configuration (airprint_mode=disabled)"
		} else {
			snapshot.Reason = "native-only mode requires a valid upstream image/urf family"
		}
		return snapshot
	}

	route, tokens, reason := emulatedURFRoute(upstream, policy)
	if reason != "" {
		snapshot.Reason = reason
		return snapshot
	}
	route.ClientFormat = "image/urf"
	route.UpstreamFormat = "image/pwg-raster"
	route.Transform = true
	route.URFTokens = tokens
	snapshot.Routes = append(snapshot.Routes, route)
	snapshot.URFTokens = append([]string(nil), tokens...)
	snapshot.AirPrintPath = AirPrintPathEmulated
	snapshot.Reason = "native URF is unavailable; exact URF-to-PWG route is eligible"
	return snapshot
}

func admittedUpstreamFormats(upstream goipp.Attributes) []string {
	attr, ok := iattr.Attr(upstream, "document-format-supported")
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(attr.Values))
	formats := make([]string, 0, len(attr.Values))
	for _, value := range attr.Values {
		if value.T != goipp.TagMimeType {
			continue
		}
		format, ok := value.V.(goipp.String)
		formatText := strings.ToLower(strings.TrimSpace(string(format)))
		if !ok || formatText == "" || formatText == "application/octet-stream" {
			continue
		}
		if _, exists := seen[formatText]; exists {
			continue
		}
		seen[formatText] = struct{}{}
		formats = append(formats, formatText)
	}
	return formats
}

func nativeURFTokens(upstream goipp.Attributes, policy config.PolicyConfig) ([]string, bool) {
	attr, ok := iattr.Attr(upstream, "urf-supported")
	if !ok || len(attr.Values) == 0 {
		return nil, false
	}
	var rawTokens []string
	for _, value := range attr.Values {
		text, ok := value.V.(goipp.String)
		if !ok {
			return nil, false
		}
		parts, valid := splitURFTokens(string(text))
		if !valid {
			return nil, false
		}
		for _, token := range parts {
			rawTokens = append(rawTokens, strings.ToUpper(token))
		}
	}
	colorMode := strings.ToLower(strings.TrimSpace(policy.PrintColorMode))
	preferredColor := ""
	if colorMode == "color" {
		for _, token := range rawTokens {
			switch {
			case strings.HasPrefix(token, "SRGB"):
				preferredColor = "SRGB"
			case preferredColor == "" && strings.HasPrefix(token, "DEVRGB"):
				preferredColor = "DEVRGB"
			}
		}
		if preferredColor == "" {
			return nil, false
		}
	}
	var tokens []string
	for _, token := range rawTokens {
		if colorMode == "monochrome" && isColorURFToken(token) {
			continue
		}
		if colorMode == "color" {
			switch {
			case strings.HasPrefix(token, "W"), strings.HasPrefix(token, "DEVW"):
				continue
			case isColorURFToken(token) && !strings.HasPrefix(token, preferredColor):
				continue
			}
		}
		tokens = append(tokens, token)
	}
	// V1.x is useful metadata but not required by the established native
	// family validator; several printers expose only width/color/resolution
	// tokens. W (for grayscale) or an exact color token, and RS, remain
	// mandatory because they select the pixel layout and resolution.
	if !containsURFPrefix(tokens, "RS") ||
		(colorMode == "monochrome" && !containsURFPrefix(tokens, "W")) ||
		(colorMode == "color" && !containsURFPrefix(tokens, preferredColor)) ||
		(colorMode != "monochrome" && colorMode != "color" && !containsURFPrefix(tokens, "W") && !isAnyColorURFToken(tokens)) {
		return nil, false
	}
	// Keep deterministic ordering for capability and DNS-SD output. The
	// original upstream ordering is not a client-visible contract.
	sort.SliceStable(tokens, func(i, j int) bool { return urfTokenRank(tokens[i]) < urfTokenRank(tokens[j]) })
	return uniqueStrings(tokens), len(tokens) > 0
}

func isAnyColorURFToken(tokens []string) bool {
	for _, token := range tokens {
		if isColorURFToken(token) {
			return true
		}
	}
	return false
}

func isColorURFToken(token string) bool {
	return strings.HasPrefix(token, "SRGB") || strings.HasPrefix(token, "ADOBERGB") ||
		strings.HasPrefix(token, "DEVRGB") || strings.HasPrefix(token, "DEVCMYK")
}

func containsURFPrefix(tokens []string, prefix string) bool {
	for _, token := range tokens {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	return false
}

func urfTokenRank(token string) int {
	for i, prefix := range []string{"V", "W", "SRGB", "DEVRGB", "DEVW", "DEVCMYK", "RS", "CP", "DM", "FN", "IS", "MT", "OB", "PQ", "IFU", "OFU"} {
		if strings.HasPrefix(token, prefix) {
			return i
		}
	}
	return 100
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func emulatedURFRoute(upstream goipp.Attributes, policy config.PolicyConfig) (DocumentRoute, []string, string) {
	if !containsMimeType(upstream, "image/pwg-raster") {
		return DocumentRoute{}, nil, "upstream does not support image/pwg-raster"
	}
	if !validPWGRasterFamily(upstream, policy) {
		return DocumentRoute{}, nil, "upstream lacks a complete compatible PWG Raster capability family"
	}
	media := buildMediaCatalog(upstream, policy)
	defaultName := policyMediaDefault(policy)
	selected, ok := media.ByName[mediaNameKey(defaultName)]
	if !ok || !selected.HasDimension || selected.XDimension <= 0 || selected.YDimension <= 0 {
		return DocumentRoute{}, nil, "configured media has no resolvable dimensions"
	}
	resolutions := squarePWGResolutions(upstream)
	if len(resolutions) == 0 {
		return DocumentRoute{}, nil, "upstream exposes no square PWG Raster resolution"
	}
	tokens := []string{"V1.4"}
	if strings.EqualFold(strings.TrimSpace(policy.PrintColorMode), "monochrome") {
		if !hasRasterType(upstream, "sgray_8") {
			return DocumentRoute{}, nil, "monochrome policy requires upstream sgray_8"
		}
		tokens = append(tokens, "W8")
	} else if strings.EqualFold(strings.TrimSpace(policy.PrintColorMode), "color") {
		if hasRasterType(upstream, "srgb_8") {
			tokens = append(tokens, "SRGB24")
		} else if hasRasterType(upstream, "rgb_8") {
			tokens = append(tokens, "DEVRGB24")
		} else {
			return DocumentRoute{}, nil, "color policy requires upstream srgb_8 or rgb_8"
		}
	} else {
		return DocumentRoute{}, nil, "AirPrint emulation requires monochrome or color policy"
	}
	tokens = append(tokens, "RS"+strconv.FormatUint(uint64(resolutions[0]), 10))
	if sheet, ok := fixedSheetBack(upstream); ok {
		return DocumentRoute{
			Mapping: mappingForPolicyAndTypes(upstream, policy),
			Page: urf.PageSettings{
				MediaName: selected.Name, MediaWidth: uint32(selected.XDimension), MediaHeight: uint32(selected.YDimension),
				MediaType: selected.MediaType, ResolutionX: resolutions[0], ResolutionY: resolutions[0],
				Sides: defaultSides(upstream), SheetBack: sheet,
			},
			Resolutions: resolutions,
		}, tokens, ""
	}
	return DocumentRoute{}, nil, "upstream has no usable PWG Raster sheet-back mode"
}

func containsMimeType(attrs goipp.Attributes, wanted string) bool {
	attr, ok := iattr.Attr(attrs, "document-format-supported")
	if !ok {
		return false
	}
	for _, value := range attr.Values {
		if value.T == goipp.TagMimeType {
			if text, ok := value.V.(goipp.String); ok && strings.EqualFold(strings.TrimSpace(string(text)), wanted) {
				return true
			}
		}
	}
	return false
}

func hasRasterType(attrs goipp.Attributes, wanted string) bool {
	attr, ok := iattr.Attr(attrs, "pwg-raster-document-type-supported")
	if !ok {
		return false
	}
	for _, value := range attr.Values {
		if text, ok := value.V.(goipp.String); ok && strings.EqualFold(strings.TrimSpace(string(text)), wanted) {
			return true
		}
	}
	return false
}

func mappingForPolicyAndTypes(attrs goipp.Attributes, policy config.PolicyConfig) urf.Mapping {
	if strings.EqualFold(strings.TrimSpace(policy.PrintColorMode), "monochrome") {
		return urf.MappingW8ToSGray8
	}
	if hasRasterType(attrs, "srgb_8") {
		return urf.MappingSRGB24ToSRGB8
	}
	return urf.MappingDEVRGB24ToRGB8
}

func squarePWGResolutions(attrs goipp.Attributes) []uint32 {
	attr, ok := iattr.Attr(attrs, "pwg-raster-document-resolution-supported")
	if !ok {
		return nil
	}
	seen := map[uint32]struct{}{}
	var result []uint32
	for _, value := range attr.Values {
		resolution, ok := value.V.(goipp.Resolution)
		if !ok || resolution.Xres <= 0 || resolution.Xres != resolution.Yres || resolution.Units != goipp.UnitsDpi {
			continue
		}
		if _, exists := seen[uint32(resolution.Xres)]; exists {
			continue
		}
		seen[uint32(resolution.Xres)] = struct{}{}
		result = append(result, uint32(resolution.Xres))
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func fixedSheetBack(attrs goipp.Attributes) (string, bool) {
	attr, ok := iattr.Attr(attrs, "pwg-raster-document-sheet-back")
	if !ok || len(attr.Values) != 1 || attr.Values[0].T != goipp.TagKeyword {
		return "", false
	}
	value, ok := attr.Values[0].V.(goipp.String)
	if !ok {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(string(value))) {
	case "normal", "flipped", "rotated", "manual-tumble":
		return strings.ToLower(strings.TrimSpace(string(value))), true
	default:
		return "", false
	}
}

func defaultSides(attrs goipp.Attributes) string {
	if value, ok := iattr.FirstString(attrs, "sides-default"); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "one-sided", "two-sided-long-edge", "two-sided-short-edge":
			return strings.ToLower(strings.TrimSpace(value))
		}
	}
	return "one-sided"
}

func defaultPageSettings(upstream goipp.Attributes, policy config.PolicyConfig) urf.PageSettings {
	settings := urf.PageSettings{MediaName: policyMediaDefault(policy), MediaType: policy.MediaType, Sides: defaultSides(upstream)}
	media := buildMediaCatalog(upstream, policy)
	if selected, ok := media.ByName[mediaNameKey(settings.MediaName)]; ok && selected.HasDimension {
		settings.MediaWidth = uint32(selected.XDimension)
		settings.MediaHeight = uint32(selected.YDimension)
		if selected.MediaType != "" {
			settings.MediaType = selected.MediaType
		}
	}
	if resolutions := squarePWGResolutions(upstream); len(resolutions) > 0 {
		settings.ResolutionX, settings.ResolutionY = resolutions[0], resolutions[0]
	}
	if quality, ok := iattr.FirstInt(upstream, "print-quality-default"); ok && quality >= 3 && quality <= 5 {
		settings.PrintQuality = uint32(quality)
	}
	if sheet, ok := fixedSheetBack(upstream); ok {
		settings.SheetBack = sheet
	}
	return settings
}

// routePageSettings overlays a route's normalized defaults with request-level
// values that are safe to carry to the translator. Policy normalization has
// already resolved media names and dropped forbidden values before this runs.
func routePageSettings(route DocumentRoute, attrs goipp.Attributes, upstream goipp.Attributes) urf.PageSettings {
	settings := route.Page
	media, ok := iattr.FirstString(attrs, "media")
	if !ok {
		// The media-col form is emitted when the queue uses collection-based
		// media policy. The normalizer has already resolved it to the policy
		// catalog; retain its named size when present so a pinned route carries
		// the same physical media across Create-Job and Send-Document.
		media = nestedStringAttribute(attrs, "media-col", "media-size-name")
		ok = media != ""
	}
	if ok {
		if selected, found := buildMediaCatalog(upstream, config.PolicyConfig{Media: media, MediaType: settings.MediaType}).ByName[mediaNameKey(media)]; found && selected.HasDimension {
			settings.MediaName = selected.Name
			settings.MediaWidth = uint32(selected.XDimension)
			settings.MediaHeight = uint32(selected.YDimension)
		}
	}
	if sides, ok := iattr.FirstString(attrs, "sides"); ok {
		settings.Sides = strings.ToLower(strings.TrimSpace(sides))
	}
	if quality, ok := iattr.FirstInt(attrs, "print-quality"); ok && quality >= 3 && quality <= 5 {
		settings.PrintQuality = uint32(quality)
	}
	if resolution, ok := singleResolution(attrs, "printer-resolution"); ok && resolution.Xres > 0 && resolution.Xres == resolution.Yres && resolution.Units == goipp.UnitsDpi {
		candidate := uint32(resolution.Xres)
		if len(route.Resolutions) == 0 || containsResolution(route.Resolutions, candidate) {
			settings.ResolutionX, settings.ResolutionY = candidate, candidate
		}
	}
	return settings
}

func singleResolution(attrs goipp.Attributes, name string) (goipp.Resolution, bool) {
	attr, ok := iattr.Attr(attrs, name)
	if !ok || len(attr.Values) != 1 || attr.Values[0].T != goipp.TagResolution {
		return goipp.Resolution{}, false
	}
	resolution, ok := attr.Values[0].V.(goipp.Resolution)
	return resolution, ok
}

func containsResolution(values []uint32, wanted uint32) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func nestedStringAttribute(attrs goipp.Attributes, collectionName, memberName string) string {
	attr, ok := iattr.Attr(attrs, collectionName)
	if !ok || len(attr.Values) != 1 {
		return ""
	}
	collection, ok := attr.Values[0].V.(goipp.Collection)
	if !ok {
		return ""
	}
	return nestedStringValue(goipp.Attributes(collection), memberName)
}

func nestedStringValue(attrs goipp.Attributes, name string) string {
	if value, ok := iattr.FirstString(attrs, name); ok {
		return strings.TrimSpace(value)
	}
	for _, attr := range attrs {
		for _, value := range attr.Values {
			if nested, ok := value.V.(goipp.Collection); ok {
				if found := nestedStringValue(goipp.Attributes(nested), name); found != "" {
					return found
				}
			}
		}
	}
	return ""
}

func formatRouteList(routes []DocumentRoute) []string {
	formats := make([]string, 0, len(routes))
	for _, route := range routes {
		formats = append(formats, route.ClientFormat)
	}
	return uniqueStrings(formats)
}

// legacyPassThroughRoute preserves the pre-snapshot behavior used by callers
// that populate Service.capabilities directly (and by rows created before the
// route model existed). A live service commits a CapabilityModel after its
// probe, so production admission still goes through the strict route family
// checks above; this fallback keeps ordinary pass-through operations and
// compatibility fixtures from requiring a synthetic capability epoch.
func legacyPassThroughRoute(upstream goipp.Attributes, format string) (DocumentRoute, bool) {
	format = strings.ToLower(strings.TrimSpace(format))
	formats := admittedLegacyFormats(upstream)
	if format == "" {
		if defaultFormat, ok := iattr.FirstString(upstream, "document-format-default"); ok {
			format = strings.ToLower(strings.TrimSpace(defaultFormat))
		}
		if format == "" && len(formats) > 0 {
			format = formats[0]
		}
	}
	if format == "" {
		return DocumentRoute{}, false
	}
	// Before a committed capability snapshot existed, ordinary IPP accepted
	// octet-stream as an opaque payload. Keep that compatibility behavior for
	// the no-model path only; committed AirPrint snapshots deliberately omit
	// the ambiguous format.
	if format == "application/octet-stream" {
		return DocumentRoute{ClientFormat: format, UpstreamFormat: format, Page: defaultPageSettings(upstream, config.PolicyConfig{})}, true
	}
	for _, candidate := range formats {
		if strings.EqualFold(candidate, format) {
			return DocumentRoute{ClientFormat: candidate, UpstreamFormat: candidate, Page: defaultPageSettings(upstream, config.PolicyConfig{})}, true
		}
	}
	return DocumentRoute{}, false
}

func admittedLegacyFormats(upstream goipp.Attributes) []string {
	attr, ok := iattr.Attr(upstream, "document-format-supported")
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(attr.Values))
	var formats []string
	for _, value := range attr.Values {
		if value.T != goipp.TagMimeType {
			continue
		}
		text, ok := value.V.(goipp.String)
		if !ok {
			continue
		}
		format := strings.ToLower(strings.TrimSpace(string(text)))
		if format == "" {
			continue
		}
		if _, exists := seen[format]; exists {
			continue
		}
		seen[format] = struct{}{}
		formats = append(formats, format)
	}
	return formats
}

// applyRouteCapabilities makes the client-facing format attributes agree with
// the committed routes. In particular, image/urf is synthesized only for an
// eligible exact route, and native URF tokens are filtered through policy so a
// monochrome queue never advertises color payloads.
func applyRouteCapabilities(base goipp.Attributes, model CapabilityModel) goipp.Attributes {
	routes := routeSnapshotForModel(model)
	formats := formatRouteList(routes.Routes)
	if len(formats) == 0 {
		return iattr.DropAttrs(base, "urf-supported")
	}
	formatValues := make([]goipp.Value, 0, len(formats))
	for _, format := range formats {
		formatValues = append(formatValues, goipp.String(format))
	}
	result := iattr.SetAttr(iattr.DropAttrs(base, "document-format-supported"), goipp.MakeAttr("document-format-supported", goipp.TagMimeType, formatValues[0], formatValues[1:]...))
	if routes.AirPrintPath == AirPrintPathNative || routes.AirPrintPath == AirPrintPathEmulated {
		result = iattr.SetAttr(iattr.DropAttrs(result, "urf-supported"), iattr.Keywords("urf-supported", routes.URFTokens...))
	} else {
		result = iattr.DropAttrs(result, "urf-supported")
	}
	if routes.AirPrintPath == AirPrintPathEmulated {
		result = applyEmulatedRasterCapabilities(result, routes)
	}
	if routes.AirPrintPath == AirPrintPathEmulated {
		if _, present := iattr.Attr(result, "document-format-default"); !present {
			result = append(result, goipp.MakeAttribute("document-format-default", goipp.TagMimeType, goipp.String("image/urf")))
		}
	}
	return result
}

func applyEmulatedRasterCapabilities(attrs goipp.Attributes, routes RouteSnapshot) goipp.Attributes {
	var emulated *DocumentRoute
	for index := range routes.Routes {
		if routes.Routes[index].Transform {
			emulated = &routes.Routes[index]
			break
		}
	}
	if emulated == nil {
		return attrs
	}
	if output := mappingOutputType(emulated.Mapping); output != "" {
		attrs = iattr.SetAttr(iattr.DropAttrs(attrs, "pwg-raster-document-type-supported"), iattr.Keyword("pwg-raster-document-type-supported", output))
	}
	if len(emulated.Resolutions) == 0 {
		return attrs
	}
	values := make([]goipp.Value, 0, len(emulated.Resolutions))
	for _, resolution := range emulated.Resolutions {
		if resolution == 0 {
			continue
		}
		values = append(values, goipp.Resolution{Xres: int(resolution), Yres: int(resolution), Units: goipp.UnitsDpi})
	}
	if len(values) > 0 {
		attrs = iattr.SetAttr(iattr.DropAttrs(attrs, "pwg-raster-document-resolution-supported"), goipp.MakeAttr("pwg-raster-document-resolution-supported", goipp.TagResolution, values[0], values[1:]...))
	}
	return attrs
}

func mappingOutputType(mapping urf.Mapping) string {
	switch mapping {
	case urf.MappingW8ToSGray8:
		return "sgray_8"
	case urf.MappingSRGB24ToSRGB8:
		return "srgb_8"
	case urf.MappingDEVRGB24ToRGB8:
		return "rgb_8"
	default:
		return ""
	}
}
