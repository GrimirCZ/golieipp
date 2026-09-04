package proxy

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

var constrainedPrinterAttrs = []string{
	"operations-supported",
	"printer-uri-supported",
	"printer-name",
	"printer-info",
	"printer-location",
	"printer-uuid",
	"printer-up-time",
	"printer-config-change-time",
	"printer-config-change-date-time",
	"media-supported",
	"media-default",
	"media-col-supported",
	"media-col-default",
	"media-col-database",
	"media-col-ready",
	"media-size-supported",
	"media-type-supported",
	"media-type-default",
	"media-source-supported",
	"media-source-default",
	"overrides-supported",
	"media-ready",
	"print-color-mode-supported",
	"print-color-mode-default",
	"color-supported",
	"output-mode-supported",
	"output-mode-default",
	"multiple-document-jobs-supported",
	"pwg-raster-document-type-supported",
	"urf-supported",
	"pages-per-minute-color",
}

// mediaSize is the normalized representation used both to advertise a
// media-col catalog and to compare media-col job requests. Dimensions are in
// IPP's hundredths of a millimetre.
type mediaSize struct {
	Name         string
	XDimension   int
	YDimension   int
	HasDimension bool

	// Margins are IPP hundredths of a millimetre. The Has* bits distinguish a
	// value copied from an upstream collection from a value filled by the
	// deterministic supported-margin fallback. Builders always emit all four
	// values, even when the upstream did not provide a collection.
	BottomMargin    int
	LeftMargin      int
	RightMargin     int
	TopMargin       int
	HasBottomMargin bool
	HasLeftMargin   bool
	HasRightMargin  bool
	HasTopMargin    bool

	// names contains media-size-name/media-key aliases seen in an upstream
	// collection. Name is always the configured policy name once a size enters
	// the filtered catalog; aliases are never emitted to clients.
	names []string
}

type mediaCatalog struct {
	Sizes  []mediaSize
	ByName map[string]mediaSize
}

type mediaCollection struct {
	size  mediaSize
	names []string
}

type mediaMargins struct {
	Bottom int
	Left   int
	Right  int
	Top    int
	Valid  bool

	// validSides lets a collection retain a valid value for one side even
	// when another side is malformed. It remains private because the filtered
	// response always emits a complete set of margins.
	validSides [4]bool
}

var pwgMediaDimensions = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)x([0-9]+(?:\.[0-9]+)?)(mm|in)(?:$|[_-])`)

func policyMediaNames(policy config.PolicyConfig) []string {
	if len(policy.MediaSupported) > 0 {
		out := make([]string, 0, len(policy.MediaSupported))
		for _, name := range policy.MediaSupported {
			name = strings.TrimSpace(name)
			if name != "" {
				out = append(out, name)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if name := strings.TrimSpace(policy.Media); name != "" {
		return []string{name}
	}
	// This fallback is useful for callers that construct a PolicyConfig
	// directly instead of loading it through config.Load.
	return []string{"iso_a4_210x297mm"}
}

func policyMediaDefault(policy config.PolicyConfig) string {
	if name := strings.TrimSpace(policy.MediaDefault); name != "" {
		return name
	}
	if name := strings.TrimSpace(policy.Media); name != "" {
		return name
	}
	names := policyMediaNames(policy)
	return names[0]
}

func mediaNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func mediaSizeFromPWGName(name string) (mediaSize, bool) {
	match := pwgMediaDimensions.FindStringSubmatch(strings.TrimSpace(name))
	if len(match) != 4 {
		return mediaSize{}, false
	}
	x, errX := strconv.ParseFloat(match[1], 64)
	y, errY := strconv.ParseFloat(match[2], 64)
	if errX != nil || errY != nil || x <= 0 || y <= 0 {
		return mediaSize{}, false
	}
	multiplier := 100.0
	if strings.EqualFold(match[3], "in") {
		multiplier = 2540.0
	}
	return mediaSize{
		Name:         strings.TrimSpace(name),
		XDimension:   int(math.Round(x * multiplier)),
		YDimension:   int(math.Round(y * multiplier)),
		HasDimension: true,
	}, true
}

func buildMediaCatalog(upstream goipp.Attributes, policy config.PolicyConfig) mediaCatalog {
	names := policyMediaNames(policy)
	upstreamCollections := upstreamMediaCollections(upstream)
	fallbackMargins := upstreamMarginDefaults(upstream)
	catalog := mediaCatalog{
		Sizes:  make([]mediaSize, 0, len(names)),
		ByName: make(map[string]mediaSize, len(names)),
	}
	for _, name := range names {
		size := mediaSize{Name: name}
		parsedPolicySize, parsed := mediaSizeFromPWGName(name)
		if parsed {
			size.XDimension = parsedPolicySize.XDimension
			size.YDimension = parsedPolicySize.YDimension
			size.HasDimension = true
		}

		// A printer may use media-size-name, media-key, or no name at all in
		// its collections. Merge every matching collection so a database
		// collection can contribute dimensions while a ready collection (or a
		// second database entry) contributes a missing margin. Exact names are
		// preferred implicitly, while dimensions provide the standard-name
		// fallback for unnamed/vendor-named collections.
		for _, collection := range upstreamCollections {
			if !mediaCollectionMatches(collection, name, parsedPolicySize, parsed) {
				continue
			}
			if collection.size.HasDimension && (!size.HasDimension || mediaCollectionHasName(collection, name)) {
				size.XDimension = collection.size.XDimension
				size.YDimension = collection.size.YDimension
				size.HasDimension = true
			}
			mergeMediaSize(&size, collection.size)
			for _, alias := range collection.names {
				if !containsMediaName(size.names, alias) {
					size.names = append(size.names, alias)
				}
			}
		}
		fillMissingMargins(&size, fallbackMargins)
		catalog.Sizes = append(catalog.Sizes, size)
		catalog.ByName[mediaNameKey(name)] = size
	}
	return catalog
}

// upstreamMediaCollections returns only collection-valued media attributes.
// Keeping the source entries, rather than flattening them by upstream name,
// lets the policy catalog match unnamed/vendor-keyed collections by physical
// dimensions and merge margins from both database and ready descriptions.
func upstreamMediaCollections(attrs goipp.Attributes) []mediaCollection {
	var result []mediaCollection
	for _, attr := range attrs {
		name := strings.ToLower(attr.Name)
		if name != "media-col-database" && name != "media-col-ready" && name != "media-size-supported" {
			continue
		}
		for _, value := range attr.Values {
			collection, ok := mediaCollectionFromValue(value.V)
			if !ok {
				continue
			}
			result = append(result, collection)
		}
	}
	return result
}

func upstreamMediaCatalog(attrs goipp.Attributes) map[string]mediaSize {
	result := make(map[string]mediaSize)
	for _, collection := range upstreamMediaCollections(attrs) {
		aliases := append([]string{}, collection.names...)
		if collection.size.Name != "" {
			aliases = append(aliases, collection.size.Name)
		}
		for _, alias := range aliases {
			key := mediaNameKey(alias)
			if key == "" {
				continue
			}
			if existing, ok := result[key]; ok {
				mergeMediaSize(&existing, collection.size)
				result[key] = existing
				continue
			}
			result[key] = collection.size
		}
	}
	return result
}

func mediaCollectionMatches(collection mediaCollection, policyName string, policySize mediaSize, hasPolicySize bool) bool {
	for _, alias := range collection.names {
		if strings.EqualFold(strings.TrimSpace(alias), strings.TrimSpace(policyName)) {
			return true
		}
	}
	if collection.size.Name != "" && strings.EqualFold(strings.TrimSpace(collection.size.Name), strings.TrimSpace(policyName)) {
		return true
	}
	return hasPolicySize && dimensionsEqual(policySize, collection.size)
}

func mediaCollectionHasName(collection mediaCollection, policyName string) bool {
	for _, alias := range collection.names {
		if strings.EqualFold(strings.TrimSpace(alias), strings.TrimSpace(policyName)) {
			return true
		}
	}
	return collection.size.Name != "" && strings.EqualFold(strings.TrimSpace(collection.size.Name), strings.TrimSpace(policyName))
}

func mergeMediaSize(dst *mediaSize, src mediaSize) {
	if !dst.HasDimension && src.HasDimension {
		dst.XDimension = src.XDimension
		dst.YDimension = src.YDimension
		dst.HasDimension = true
	}
	if !dst.HasBottomMargin && src.HasBottomMargin {
		dst.BottomMargin = src.BottomMargin
		dst.HasBottomMargin = true
	}
	if !dst.HasLeftMargin && src.HasLeftMargin {
		dst.LeftMargin = src.LeftMargin
		dst.HasLeftMargin = true
	}
	if !dst.HasRightMargin && src.HasRightMargin {
		dst.RightMargin = src.RightMargin
		dst.HasRightMargin = true
	}
	if !dst.HasTopMargin && src.HasTopMargin {
		dst.TopMargin = src.TopMargin
		dst.HasTopMargin = true
	}
}

func fillMissingMargins(size *mediaSize, fallback mediaMargins) {
	if !size.HasBottomMargin {
		size.BottomMargin = fallback.Bottom
	}
	if !size.HasLeftMargin {
		size.LeftMargin = fallback.Left
	}
	if !size.HasRightMargin {
		size.RightMargin = fallback.Right
	}
	if !size.HasTopMargin {
		size.TopMargin = fallback.Top
	}
}

// upstreamMarginDefaults chooses the smallest non-negative fixed integer
// advertised by each global margin-supported attribute. IPP media margins
// are physical non-negative lengths in hundredths of a millimetre, so zero is
// a standards-safe compatibility fallback when a printer omits the attribute
// or sends a range/malformed value instead of a fixed integer. We intentionally
// do not claim an unreported non-zero margin.
func upstreamMarginDefaults(attrs goipp.Attributes) mediaMargins {
	bottom, bottomOK := minimumSupportedMargin(attrs, "media-bottom-margin-supported")
	left, leftOK := minimumSupportedMargin(attrs, "media-left-margin-supported")
	right, rightOK := minimumSupportedMargin(attrs, "media-right-margin-supported")
	top, topOK := minimumSupportedMargin(attrs, "media-top-margin-supported")
	return mediaMargins{
		Bottom: bottom,
		Left:   left,
		Right:  right,
		Top:    top,
		Valid:  bottomOK && leftOK && rightOK && topOK,
	}
}

func minimumSupportedMargin(attrs goipp.Attributes, name string) (int, bool) {
	attr, ok := iattr.Attr(attrs, name)
	if !ok {
		return 0, false
	}
	minimum, found := 0, false
	for _, value := range attr.Values {
		candidate, ok := fixedNonNegativeInteger(goipp.Values{value})
		if !ok {
			continue
		}
		if !found || candidate < minimum {
			minimum, found = candidate, true
		}
	}
	return minimum, found
}

// mediaSizeFromCollection accepts both common IPP representations: a
// media-col collection whose media-size is nested below it, and a bare
// media-size collection. Some printers put media-key/media-size-name inside
// media-size while others put it beside media-size, so members are searched
// recursively.
func mediaSizeFromCollection(value goipp.Value) (mediaSize, bool) {
	collection, ok := mediaCollectionFromValue(value)
	if !ok {
		return mediaSize{}, false
	}
	return collection.size, true
}

func mediaSizeFromCollectionAttrs(attrs goipp.Attributes) (mediaSize, bool) {
	collection := mediaCollectionFromAttrs(attrs)
	return collection.size, len(collection.names) > 0 || collection.size.HasDimension
}

func mediaCollectionFromValue(value goipp.Value) (mediaCollection, bool) {
	collection, ok := value.(goipp.Collection)
	if !ok {
		return mediaCollection{}, false
	}
	result := mediaCollectionFromAttrs(goipp.Attributes(collection))
	return result, len(result.names) > 0 || result.size.HasDimension
}

func mediaCollectionFromAttrs(attrs goipp.Attributes) mediaCollection {
	result := mediaCollection{}
	var mediaSizeName, mediaKey string

	var walk func(goipp.Attributes)
	walk = func(current goipp.Attributes) {
		for _, attr := range current {
			name := strings.ToLower(attr.Name)
			switch name {
			case "media-key", "media-size-name":
				for _, value := range attr.Values {
					text, ok := stringValue(value.V)
					text = strings.TrimSpace(text)
					if !ok || text == "" || containsMediaName(result.names, text) {
						continue
					}
					result.names = append(result.names, text)
					if name == "media-size-name" && mediaSizeName == "" {
						mediaSizeName = text
					}
					if name == "media-key" && mediaKey == "" {
						mediaKey = text
					}
				}
			case "x-dimension":
				if value, ok := fixedInteger(attr.Values); ok && value > 0 && result.size.XDimension == 0 {
					result.size.XDimension = value
				}
			case "y-dimension":
				if value, ok := fixedInteger(attr.Values); ok && value > 0 && result.size.YDimension == 0 {
					result.size.YDimension = value
				}
			case "media-bottom-margin":
				if value, ok := fixedNonNegativeInteger(attr.Values); ok && !result.size.HasBottomMargin {
					result.size.BottomMargin, result.size.HasBottomMargin = value, true
				}
			case "media-left-margin":
				if value, ok := fixedNonNegativeInteger(attr.Values); ok && !result.size.HasLeftMargin {
					result.size.LeftMargin, result.size.HasLeftMargin = value, true
				}
			case "media-right-margin":
				if value, ok := fixedNonNegativeInteger(attr.Values); ok && !result.size.HasRightMargin {
					result.size.RightMargin, result.size.HasRightMargin = value, true
				}
			case "media-top-margin":
				if value, ok := fixedNonNegativeInteger(attr.Values); ok && !result.size.HasTopMargin {
					result.size.TopMargin, result.size.HasTopMargin = value, true
				}
			}
			for _, value := range attr.Values {
				if nested, ok := value.V.(goipp.Collection); ok {
					walk(goipp.Attributes(nested))
				}
			}
		}
	}
	walk(attrs)
	if mediaSizeName != "" {
		result.size.Name = mediaSizeName
	} else if mediaKey != "" {
		result.size.Name = mediaKey
	} else if len(result.names) > 0 {
		result.size.Name = result.names[0]
	}
	result.size.HasDimension = result.size.XDimension > 0 && result.size.YDimension > 0
	result.size.names = append([]string{}, result.names...)
	return result
}

func containsMediaName(names []string, wanted string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}

func stringValue(value goipp.Value) (string, bool) {
	if value == nil {
		return "", false
	}
	text, ok := value.(goipp.String)
	return string(text), ok
}

func firstValueString(values goipp.Values) (string, bool) {
	if len(values) == 0 || values[0].V == nil {
		return "", false
	}
	switch value := values[0].V.(type) {
	case goipp.String:
		return string(value), true
	default:
		return fmt.Sprint(value), true
	}
}

func fixedInteger(values goipp.Values) (int, bool) {
	if len(values) != 1 {
		return 0, false
	}
	switch value := values[0].V.(type) {
	case goipp.Integer:
		return int(value), true
	case goipp.Range:
		if value.Lower == value.Upper {
			return value.Lower, true
		}
	}
	return 0, false
}

func fixedNonNegativeInteger(values goipp.Values) (int, bool) {
	if len(values) != 1 {
		return 0, false
	}
	// Margin members and media-*-margin-supported are integer/1setOf
	// integer attributes. A range, even an equal-endpoint range, is a
	// malformed representation here and must not be copied as a fixed value.
	value, ok := values[0].V.(goipp.Integer)
	return int(value), ok && value >= 0
}

// FilterPrinterAttributes removes upstream policy/identity values and adds
// proxy-owned values. The optional metadata argument preserves compatibility
// for callers that only need policy filtering; Service always supplies its
// process-owned metadata so repeated capability queries share one identity and
// one configuration-change epoch.
func FilterPrinterAttributes(upstream goipp.Attributes, queueName, proxyURI string, printer config.PrinterConfig, metadata ...PrinterIdentityMetadata) goipp.Attributes {
	out := iattr.DropAttrs(upstream, constrainedPrinterAttrs...)
	identity := defaultPrinterIdentityMetadata(proxyURI)
	if len(metadata) > 0 {
		identity = metadata[0]
	}
	if identity.UUID == "" {
		identity.UUID = proxyPrinterUUID(proxyURI)
	}
	if identity.ConfigChangeDateTime.IsZero() {
		identity.ConfigChangeDateTime = defaultPrinterIdentityMetadata(proxyURI).ConfigChangeDateTime
	}
	out = append(out,
		iattr.URI("printer-uri-supported", proxyURI),
		iattr.Name("printer-name", printer.DisplayName),
		iattr.Text("printer-info", fmt.Sprintf("%s via policy proxy", printer.DisplayName)),
		iattr.Text("printer-location", printer.Location),
	)
	out = append(out, proxyPrinterIdentityAttributes(identity)...)

	mediaNames := policyMediaNames(printer.Policy)
	mediaDefault := policyMediaDefault(printer.Policy)
	catalog := buildMediaCatalog(upstream, printer.Policy)
	mediaSource := policyMediaSource(printer.Policy)
	mediaDatabase := mediaColDatabaseAttrWithSource(catalog.Sizes, printer.Policy.MediaType, mediaSource)
	defaultSize, defaultFound := catalog.ByName[mediaNameKey(mediaDefault)]
	// If a caller supplied a policy whose default is not represented in the
	// catalog (possible before config validation), use the first item rather
	// than emitting an empty collection.
	if !defaultFound && len(catalog.Sizes) > 0 {
		defaultSize = catalog.Sizes[0]
	}
	mediaDefaultCol := mediaColDescriptionAttr("media-col-default", defaultSize, printer.Policy.MediaType, mediaSource)
	mediaSizeSupported, hasMediaSizeSupported := mediaSizeSupportedAttr(catalog.Sizes)
	out = append(out,
		iattr.Keywords("media-supported", mediaNames...),
		iattr.Keyword("media-default", mediaDefault),
		iattr.Keywords("media-col-supported", mediaColSupportedNames(catalog, printer.Policy)...),
		mediaDefaultCol,
		mediaDatabase,
		iattr.Keyword("media-type-supported", printer.Policy.MediaType),
		iattr.Keyword("media-type-default", printer.Policy.MediaType),
		iattr.Keyword("print-color-mode-supported", printer.Policy.PrintColorMode),
		iattr.Keyword("print-color-mode-default", printer.Policy.PrintColorMode),
		iattr.Boolean("color-supported", false),
		iattr.Keyword("output-mode-supported", "monochrome"),
		iattr.Keyword("output-mode-default", "monochrome"),
		operationsSupportedAttr(),
		overridesSupportedAttr(),
		iattr.Boolean("multiple-document-jobs-supported", false),
		iattr.Keyword("pwg-raster-document-type-supported", "sgray_8"),
	)
	if hasMediaSizeSupported {
		out = append(out, mediaSizeSupported)
	}
	// A nil media-source policy means that the proxy deliberately leaves tray
	// selection automatic. IPP Everywhere still requires a standards-valid
	// media-source-supported attribute, so advertise the standard "auto"
	// keyword rather than omitting the attribute or leaking an upstream tray
	// identifier that the policy does not enforce.
	advertisedMediaSource := policyMediaSource(printer.Policy)
	if advertisedMediaSource == "" {
		advertisedMediaSource = "auto"
	}
	out = append(out,
		iattr.Keyword("media-source-supported", advertisedMediaSource),
		iattr.Keyword("media-source-default", advertisedMediaSource),
	)
	out = append(out, filteredReadyAttributes(upstream, catalog, printer.Policy)...)
	_ = queueName
	return out
}

func policyMediaSource(policy config.PolicyConfig) string {
	if policy.MediaSource == nil {
		return ""
	}
	return strings.TrimSpace(*policy.MediaSource)
}

func mediaColSupportedNames(catalog mediaCatalog, policy config.PolicyConfig) []string {
	result := make([]string, 0, 8)
	for _, size := range catalog.Sizes {
		if validMediaSizeName(size.Name) {
			result = append(result, "media-size-name")
			break
		}
	}
	for _, size := range catalog.Sizes {
		if size.HasDimension {
			result = append(result, "media-size")
			break
		}
	}
	if policy.MediaType != "" {
		result = append(result, "media-type")
	}
	// The proxy always emits complete fixed margins in printer-description
	// collections, including when they came from the zero fallback.
	result = append(result,
		"media-bottom-margin",
		"media-left-margin",
		"media-right-margin",
		"media-top-margin",
	)
	if policyMediaSource(policy) != "" {
		result = append(result, "media-source")
	}
	return result
}

func validMediaSizeName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && !strings.ContainsAny(name, " \t\r\n")
}

func mediaSizeSupportedAttr(sizes []mediaSize) (goipp.Attribute, bool) {
	values := make([]goipp.Value, 0, len(sizes))
	for _, size := range sizes {
		if !size.HasDimension {
			continue
		}
		values = append(values, goipp.Collection{
			iattr.Integer("x-dimension", size.XDimension),
			iattr.Integer("y-dimension", size.YDimension),
		})
	}
	if len(values) == 0 {
		return goipp.Attribute{}, false
	}
	return goipp.MakeAttr("media-size-supported", goipp.TagBeginCollection, values[0], values[1:]...), true
}

// mediaColDescriptionAttr builds a media-col member for printer-description
// attributes. Unlike a job request, a description may identify a policy media
// by media-size-name. It never copies an upstream media-key.
func mediaColDescriptionAttr(name string, size mediaSize, mediaType, mediaSource string) goipp.Attribute {
	members := make(goipp.Attributes, 0, 9)
	if validMediaSizeName(size.Name) {
		members = append(members, iattr.Keyword("media-size-name", size.Name))
	}
	mediaSizeMembers := make(goipp.Attributes, 0, 2)
	if size.HasDimension {
		mediaSizeMembers = append(mediaSizeMembers,
			iattr.Integer("x-dimension", size.XDimension),
			iattr.Integer("y-dimension", size.YDimension),
		)
	}
	if len(mediaSizeMembers) > 0 {
		members = append(members, goipp.MakeAttrCollection("media-size", mediaSizeMembers[0], mediaSizeMembers[1:]...))
	}
	if mediaType != "" {
		members = append(members, iattr.Keyword("media-type", mediaType))
	}
	if mediaSource != "" {
		members = append(members, iattr.Keyword("media-source", mediaSource))
	}
	members = append(members,
		iattr.Integer("media-bottom-margin", size.BottomMargin),
		iattr.Integer("media-left-margin", size.LeftMargin),
		iattr.Integer("media-right-margin", size.RightMargin),
		iattr.Integer("media-top-margin", size.TopMargin),
	)
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, goipp.Collection(members))
}

// mediaColAttr is retained as the package-local description builder used by
// existing callers/tests; new code should use the explicit description/job
// names so a printer identifier cannot accidentally enter a job request.
func mediaColAttr(name string, size mediaSize, mediaType string) goipp.Attribute {
	return mediaColDescriptionAttr(name, size, mediaType, "")
}

// mediaColJobAttr builds the media-col value sent in a job-creation request.
// media-size-name and media-key are printer-description identifiers and must
// not be combined with media-size in a request. The proxy therefore sends
// only fixed dimensions, plus the policy's type/source when configured.
func mediaColJobAttr(name string, size mediaSize, mediaType, mediaSource string) goipp.Attribute {
	members := make(goipp.Attributes, 0, 3)
	mediaSizeMembers := make(goipp.Attributes, 0, 2)
	if size.HasDimension {
		mediaSizeMembers = append(mediaSizeMembers,
			iattr.Integer("x-dimension", size.XDimension),
			iattr.Integer("y-dimension", size.YDimension),
		)
	}
	if len(mediaSizeMembers) > 0 {
		members = append(members, goipp.MakeAttrCollection("media-size", mediaSizeMembers[0], mediaSizeMembers[1:]...))
	}
	if mediaType != "" {
		members = append(members, iattr.Keyword("media-type", mediaType))
	}
	if mediaSource != "" {
		members = append(members, iattr.Keyword("media-source", mediaSource))
	}
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, goipp.Collection(members))
}

func mediaColDatabaseAttrWithSource(sizes []mediaSize, mediaType, mediaSource string) goipp.Attribute {
	if len(sizes) == 0 {
		return goipp.Attribute{Name: "media-col-database"}
	}
	values := make([]goipp.Value, 0, len(sizes))
	for _, size := range sizes {
		attr := mediaColDescriptionAttr("media-col-database", size, mediaType, mediaSource)
		if len(attr.Values) > 0 {
			values = append(values, attr.Values[0].V)
		}
	}
	if len(values) == 0 {
		return goipp.Attribute{Name: "media-col-database"}
	}
	return goipp.MakeAttr("media-col-database", goipp.TagBeginCollection, values[0], values[1:]...)
}

func mediaColDatabaseAttr(sizes []mediaSize, mediaType string) goipp.Attribute {
	return mediaColDatabaseAttrWithSource(sizes, mediaType, "")
}

func mediaColReadyAttr(sizes []mediaSize, mediaType, mediaSource string) goipp.Attribute {
	if len(sizes) == 0 {
		return goipp.Attribute{Name: "media-col-ready"}
	}
	values := make([]goipp.Value, 0, len(sizes))
	for _, size := range sizes {
		attr := mediaColDescriptionAttr("media-col-ready", size, mediaType, mediaSource)
		if len(attr.Values) > 0 {
			values = append(values, attr.Values[0].V)
		}
	}
	if len(values) == 0 {
		return goipp.Attribute{Name: "media-col-ready"}
	}
	return goipp.MakeAttr("media-col-ready", goipp.TagBeginCollection, values[0], values[1:]...)
}

func filteredReadyAttributes(upstream goipp.Attributes, catalog mediaCatalog, policy config.PolicyConfig) goipp.Attributes {
	var result goipp.Attributes
	if attr, ok := iattr.Attr(upstream, "media-ready"); ok {
		upstreamReady := make(map[string]struct{})
		if values, valid := attributeStrings(attr); valid {
			for _, value := range values {
				upstreamReady[mediaNameKey(value)] = struct{}{}
			}
		}
		readyNames := make([]string, 0, len(catalog.Sizes))
		for _, size := range catalog.Sizes {
			if _, ready := upstreamReady[mediaNameKey(size.Name)]; ready {
				readyNames = append(readyNames, size.Name)
			}
		}
		if len(readyNames) > 0 {
			result = append(result, iattr.Keywords("media-ready", readyNames...))
		}
	}

	if attr, ok := iattr.Attr(upstream, "media-col-ready"); ok {
		readyByName := make(map[string]mediaSize)
		for _, value := range attr.Values {
			collection, valid := mediaCollectionFromValue(value.V)
			if !valid {
				continue
			}
			index := catalogIndexForCollection(collection, catalog)
			if index < 0 {
				continue
			}
			resolved := catalog.Sizes[index]
			overlayMediaSize(&resolved, collection.size)
			readyByName[mediaNameKey(resolved.Name)] = resolved
		}
		readySizes := make([]mediaSize, 0, len(catalog.Sizes))
		for _, size := range catalog.Sizes {
			if ready, found := readyByName[mediaNameKey(size.Name)]; found {
				readySizes = append(readySizes, ready)
			}
		}
		if len(readySizes) > 0 {
			result = append(result, mediaColReadyAttr(readySizes, policy.MediaType, policyMediaSource(policy)))
		}
	}
	return result
}

func catalogIndexForCollection(collection mediaCollection, catalog mediaCatalog) int {
	for index, size := range catalog.Sizes {
		for _, alias := range collection.names {
			if strings.EqualFold(strings.TrimSpace(alias), strings.TrimSpace(size.Name)) {
				return index
			}
		}
		if collection.size.Name != "" && strings.EqualFold(strings.TrimSpace(collection.size.Name), strings.TrimSpace(size.Name)) {
			return index
		}
	}
	if collection.size.HasDimension {
		for index, size := range catalog.Sizes {
			if dimensionsEqual(collection.size, size) {
				return index
			}
		}
	}
	return -1
}

func overlayMediaSize(dst *mediaSize, src mediaSize) {
	if src.HasDimension {
		dst.XDimension = src.XDimension
		dst.YDimension = src.YDimension
		dst.HasDimension = true
	}
	if src.HasBottomMargin {
		dst.BottomMargin, dst.HasBottomMargin = src.BottomMargin, true
	}
	if src.HasLeftMargin {
		dst.LeftMargin, dst.HasLeftMargin = src.LeftMargin, true
	}
	if src.HasRightMargin {
		dst.RightMargin, dst.HasRightMargin = src.RightMargin, true
	}
	if src.HasTopMargin {
		dst.TopMargin, dst.HasTopMargin = src.TopMargin, true
	}
}

func operationsSupportedAttr() goipp.Attribute {
	ops := []goipp.Op{
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
	}
	values := make([]goipp.Value, len(ops))
	for i, op := range ops {
		values[i] = goipp.Integer(op)
	}
	return goipp.MakeAttr("operations-supported", goipp.TagEnum, values[0], values[1:]...)
}

// overridesSupportedAttr is proxy-owned. The proxy forwards ordinary
// document/page overrides, but policy-controlled members such as media and
// media-col are removed from incoming override collections (see
// sanitizeOverrides). Advertising only these two target forms avoids
// propagating an upstream printer's non-standard "document-numbers" value.
func overridesSupportedAttr() goipp.Attribute {
	return iattr.Keywords("overrides-supported", "document-number", "pages")
}

func ValidatePolicyAgainstUpstream(attrs goipp.Attributes, printer config.PrinterConfig) error {
	for _, media := range policyMediaNames(printer.Policy) {
		if printer.Policy.UseMediaCol {
			if !upstreamSupportsMedia(attrs, media) {
				return fmt.Errorf("upstream does not advertise media-supported=%s", media)
			}
			continue
		}
		supported, ok := iattr.Attr(attrs, "media-supported")
		if !ok || !hasString(supported, media) {
			return fmt.Errorf("upstream does not advertise media-supported=%s", media)
		}
	}
	if attr, ok := iattr.Attr(attrs, "print-color-mode-supported"); ok {
		if !hasString(attr, printer.Policy.PrintColorMode) {
			return fmt.Errorf("upstream does not advertise print-color-mode-supported=%s", printer.Policy.PrintColorMode)
		}
	}
	if printer.Policy.MediaType != "" {
		if attr, ok := iattr.Attr(attrs, "media-type-supported"); ok && !hasString(attr, printer.Policy.MediaType) {
			return fmt.Errorf("upstream does not advertise media-type-supported=%s", printer.Policy.MediaType)
		}
	}
	if source := policyMediaSource(printer.Policy); source != "" {
		if attr, ok := iattr.Attr(attrs, "media-source-supported"); !ok || !hasString(attr, source) {
			return fmt.Errorf("upstream does not advertise media-source-supported=%s", source)
		}
	}
	// The proxy emits media-col-default/database for every policy regardless of
	// whether jobs use media-col. Refuse a custom policy that cannot produce
	// standards-valid fixed dimensions instead of advertising an incomplete
	// catalog that driverless clients silently discard.
	catalog := buildMediaCatalog(attrs, printer.Policy)
	for _, size := range catalog.Sizes {
		if !size.HasDimension {
			return fmt.Errorf("cannot resolve fixed dimensions for media-supported=%s", size.Name)
		}
	}
	return nil
}

func upstreamSupportsMedia(attrs goipp.Attributes, policyName string) bool {
	if supported, ok := iattr.Attr(attrs, "media-supported"); ok && hasString(supported, policyName) {
		return true
	}
	parsed, parsedOK := mediaSizeFromPWGName(policyName)
	for _, collection := range upstreamMediaCollections(attrs) {
		if mediaCollectionMatches(collection, policyName, parsed, parsedOK) {
			return true
		}
	}
	return false
}

func hasString(attr goipp.Attribute, value string) bool {
	for _, val := range attr.Values {
		if s, ok := val.V.(goipp.String); ok && strings.EqualFold(string(s), value) {
			return true
		}
	}
	return false
}

func dimensionsEqual(a, b mediaSize) bool {
	if !a.HasDimension || !b.HasDimension {
		return false
	}
	return (a.XDimension == b.XDimension && a.YDimension == b.YDimension) ||
		(a.XDimension == b.YDimension && a.YDimension == b.XDimension)
}
