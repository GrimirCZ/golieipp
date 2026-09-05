package proxy

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

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
	"pwg-raster-document-resolution-supported",
	"pwg-raster-document-sheet-back",
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

	// MediaType and MediaSource are part of the media instance identity. A
	// size alone is not enough to describe a printer: the same sheet may be
	// loaded in several trays, with different stock types or margins. The
	// presence bits distinguish an explicitly reported empty value from an
	// attribute that was not reported at all.
	MediaType      string
	MediaSource    string
	HasMediaType   bool
	HasMediaSource bool
	Ready          bool

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
	// Sizes is the complete filtered catalog. It intentionally contains one
	// entry per physical media instance, so two trays carrying the same size
	// are not accidentally merged. Instances is an alias-like view retained
	// for callers that want to make that intent explicit.
	Sizes     []mediaSize
	Instances []mediaSize
	ByName    map[string]mediaSize
	ByNameAll map[string][]mediaSize
}

type mediaCollection struct {
	size  mediaSize
	names []string
	ready bool
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
	xDimension, validX := roundedIPPDimension(x, multiplier)
	yDimension, validY := roundedIPPDimension(y, multiplier)
	if !validX || !validY {
		return mediaSize{}, false
	}
	return mediaSize{
		Name:         strings.TrimSpace(name),
		XDimension:   xDimension,
		YDimension:   yDimension,
		HasDimension: true,
	}, true
}

// roundedIPPDimension converts a PWG media-name measurement to IPP's
// integer hundredths-of-a-millimetre representation. Reject values that
// round to zero or cannot be represented by goipp.Integer (int32), rather
// than allowing an overflow or a non-positive dimension into a collection.
func roundedIPPDimension(value, multiplier float64) (int, bool) {
	if value <= 0 || multiplier <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	scaled := value * multiplier
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled < 1 || scaled > float64(math.MaxInt32) {
		return 0, false
	}
	rounded := math.Round(scaled)
	if rounded < 1 || rounded > float64(math.MaxInt32) {
		return 0, false
	}
	return int(rounded), true
}

func buildMediaCatalog(upstream goipp.Attributes, policy config.PolicyConfig) mediaCatalog {
	names := policyMediaNames(policy)
	upstreamCollections := upstreamMediaCollections(upstream)
	catalog := mediaCatalog{
		Sizes:     make([]mediaSize, 0, len(names)),
		Instances: make([]mediaSize, 0, len(names)),
		ByName:    make(map[string]mediaSize, len(names)),
		ByNameAll: make(map[string][]mediaSize, len(names)),
	}
	configuredSource := policyMediaSource(policy)
	for _, name := range names {
		base := mediaSize{Name: name}
		parsedPolicySize, parsed := mediaSizeFromPWGName(name)
		if parsed {
			base.XDimension = parsedPolicySize.XDimension
			base.YDimension = parsedPolicySize.YDimension
			base.HasDimension = true
		}

		// A printer may use media-size-name, media-key, or no name at all in
		// its collections. Keep every matching instance: a second tray or
		// stock type is not merely another spelling of the first one. Database
		// and ready entries describing the same physical instance are merged
		// by source/type/dimensions while retaining the ready bit.
		instances := make([]mediaSize, 0, len(upstreamCollections))
		for _, collection := range upstreamCollections {
			if !mediaCollectionMatches(collection, name, parsedPolicySize, parsed) {
				continue
			}
			if configuredSource != "" && collection.size.HasMediaSource && !strings.EqualFold(collection.size.MediaSource, configuredSource) {
				continue
			}
			candidate := base
			if collection.size.HasDimension && (!candidate.HasDimension || mediaCollectionHasName(collection, name)) {
				candidate.XDimension = collection.size.XDimension
				candidate.YDimension = collection.size.YDimension
				candidate.HasDimension = true
			}
			mergeMediaSize(&candidate, collection.size)
			candidate.Name = name
			for _, alias := range collection.names {
				if !containsMediaName(candidate.names, alias) {
					candidate.names = append(candidate.names, alias)
				}
			}
			if collection.ready {
				candidate.Ready = true
			}
			instances = mergeMediaInstance(instances, candidate)
		}
		if len(instances) == 0 {
			// Standard PWG names carry fixed dimensions. For a custom name we
			// retain an unresolved logical entry so validation can reject it;
			// do not fabricate margins, source, or type values.
			instances = append(instances, base)
		}
		for index := range instances {
			instance := &instances[index]
			if !instance.HasMediaSource {
				if source := provenAutomaticMediaSource(upstream); source != "" {
					instance.MediaSource = source
					instance.HasMediaSource = true
				}
			}
			if !instance.HasMediaType && strings.TrimSpace(policy.MediaType) != "" {
				instance.MediaType = strings.TrimSpace(policy.MediaType)
				instance.HasMediaType = true
			}
		}
		key := mediaNameKey(name)
		catalog.ByNameAll[key] = append(catalog.ByNameAll[key], instances...)
		catalog.Sizes = append(catalog.Sizes, instances...)
		catalog.Instances = append(catalog.Instances, instances...)
		catalog.ByName[key] = preferredMediaInstance(instances)
	}
	return catalog
}

// mergeMediaInstance merges a database and ready description that refer to
// the same physical instance. It deliberately does not merge entries that
// differ by source or media type, even if their dimensions are identical.
func mergeMediaInstance(instances []mediaSize, candidate mediaSize) []mediaSize {
	for index := range instances {
		if !sameMediaInstance(instances[index], candidate) {
			continue
		}
		mergeMediaSize(&instances[index], candidate)
		instances[index].Ready = instances[index].Ready || candidate.Ready
		return instances
	}
	return append(instances, candidate)
}

func sameMediaInstance(a, b mediaSize) bool {
	if a.HasMediaSource != b.HasMediaSource || (a.HasMediaSource && !strings.EqualFold(a.MediaSource, b.MediaSource)) {
		return false
	}
	if a.HasMediaType != b.HasMediaType || (a.HasMediaType && !strings.EqualFold(a.MediaType, b.MediaType)) {
		return false
	}
	if a.HasDimension && b.HasDimension && !dimensionsEqual(a, b) {
		return false
	}
	if a.HasBottomMargin && b.HasBottomMargin && a.BottomMargin != b.BottomMargin {
		return false
	}
	if a.HasLeftMargin && b.HasLeftMargin && a.LeftMargin != b.LeftMargin {
		return false
	}
	if a.HasRightMargin && b.HasRightMargin && a.RightMargin != b.RightMargin {
		return false
	}
	if a.HasTopMargin && b.HasTopMargin && a.TopMargin != b.TopMargin {
		return false
	}
	return true
}

func preferredMediaInstance(instances []mediaSize) mediaSize {
	if len(instances) == 0 {
		return mediaSize{}
	}
	best := instances[0]
	bestScore := mediaInstancePreference(best)
	for _, candidate := range instances[1:] {
		if score := mediaInstancePreference(candidate); score > bestScore {
			best, bestScore = candidate, score
		}
	}
	return best
}

func mediaInstancePreference(size mediaSize) int {
	score := 0
	if size.Ready {
		score += 100
	}
	if mediaSizeHasNonZeroMargin(size) {
		score += 10
	}
	return score
}

func mediaSizeHasNonZeroMargin(size mediaSize) bool {
	return (size.HasBottomMargin && size.BottomMargin > 0) ||
		(size.HasLeftMargin && size.LeftMargin > 0) ||
		(size.HasRightMargin && size.RightMargin > 0) ||
		(size.HasTopMargin && size.TopMargin > 0)
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
			collection.ready = name == "media-col-ready"
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
	if !dst.HasMediaType && src.HasMediaType {
		dst.MediaType = src.MediaType
		dst.HasMediaType = true
	}
	if !dst.HasMediaSource && src.HasMediaSource {
		dst.MediaSource = src.MediaSource
		dst.HasMediaSource = true
	}
	dst.Ready = dst.Ready || src.Ready
	for _, alias := range src.names {
		if !containsMediaName(dst.names, alias) {
			dst.names = append(dst.names, alias)
		}
	}
}

func fillMissingMargins(size *mediaSize, fallback mediaMargins) {
	// Unknown margins are intentionally left unknown. A zero value in an IPP
	// media collection means borderless/full-bleed media; it is not a safe
	// default for a missing member. Keep this helper for source compatibility
	// with older callers, but do not copy the fallback into the instance.
	_ = size
	_ = fallback
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
			case "media-type":
				if value, ok := fixedKeyword(attr.Values); ok && value != "" && !result.size.HasMediaType {
					result.size.MediaType = value
					result.size.HasMediaType = true
				}
			case "media-source":
				if value, ok := fixedKeyword(attr.Values); ok && value != "" && !result.size.HasMediaSource {
					result.size.MediaSource = value
					result.size.HasMediaSource = true
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

func fixedKeyword(values goipp.Values) (string, bool) {
	if len(values) != 1 || values[0].V == nil {
		return "", false
	}
	if values[0].T != goipp.TagKeyword && values[0].T != goipp.TagName {
		return "", false
	}
	value, ok := values[0].V.(goipp.String)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(string(value)), true
}

// CapabilityModel is the Capability Model seam used by the service and
// discovery adapters. It keeps the upstream snapshot separate from the
// synthesized client view, while making feature eligibility explicit instead
// of allowing an upstream ipp-features-supported value to leak through.
type CapabilityModel struct {
	Upstream   goipp.Attributes
	Printer    config.PrinterConfig
	Policy     config.PolicyConfig
	QueueName  string
	ProxyURI   string
	Identity   PrinterIdentityMetadata
	Operations []goipp.Op

	Disabled            bool
	RequiredOperations  []goipp.Op
	RequiredUnsatisfied []goipp.Op
	IPPEligible         bool
	IPPFeatures         []string
	Attributes          goipp.Attributes
}

// CapabilityModelOptions supplies the dynamic operation surface. A nil
// Operations slice uses the proxy's normal operation set; an explicitly empty
// non-nil slice represents a disabled operation surface.
type CapabilityModelOptions struct {
	Operations []goipp.Op
	Disabled   bool
}

var proxyOperations = []goipp.Op{
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

var ippEverywhereRequiredOperations = []goipp.Op{
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

// NewCapabilityModel creates and immediately synthesizes a capability model.
func NewCapabilityModel(upstream goipp.Attributes, queueName, proxyURI string, printer config.PrinterConfig, options CapabilityModelOptions, metadata ...PrinterIdentityMetadata) CapabilityModel {
	var operations []goipp.Op
	if options.Operations != nil {
		operations = append([]goipp.Op{}, options.Operations...)
	}
	model := CapabilityModel{
		Upstream:   upstream.DeepCopy(),
		Printer:    printer,
		Policy:     printer.Policy,
		QueueName:  queueName,
		ProxyURI:   proxyURI,
		Disabled:   options.Disabled,
		Operations: operations,
	}
	if len(metadata) > 0 {
		model.Identity = metadata[0]
	}
	return SynthesizeCapabilityModel(model)
}

// SynthesizeCapabilityModel computes operation/feature eligibility and the
// complete synthesized printer-description attribute set.
func SynthesizeCapabilityModel(model CapabilityModel) CapabilityModel {
	if policyConfigEmpty(model.Policy) {
		model.Policy = model.Printer.Policy
	}
	if policyConfigEmpty(model.Printer.Policy) {
		model.Printer.Policy = model.Policy
	}
	// The printer configuration is part of the model contract. Keep direct
	// callers consistent with the service path, which already turns this mode
	// into a disabled capability surface before synthesis.
	if model.Printer.IPPEverywhereMode == config.IPPEverywhereDisabled {
		model.Disabled = true
	}
	// A non-nil empty operation set is an explicit disabled operation surface;
	// do not let it fall through to the default proxy operation set.
	if model.Operations != nil && len(model.Operations) == 0 {
		model.Disabled = true
	}
	if model.Operations == nil {
		model.Operations = append([]goipp.Op(nil), proxyOperations...)
	}
	model.Operations = uniqueOperations(model.Operations)
	model.RequiredOperations = append([]goipp.Op(nil), ippEverywhereRequiredOperations...)
	model.RequiredUnsatisfied = missingOperations(model.Operations, model.RequiredOperations)
	// Eligibility deliberately trusts the upstream ipp-everywhere claim. The
	// missing-operation list is diagnostic only; this proxy's advertisement is
	// a compatibility signal, not a self-certification result.
	model.IPPEligible = !model.Disabled && upstreamClaimsIPPEverywhere(model.Upstream)
	model.IPPFeatures = nil
	if model.IPPEligible {
		model.IPPFeatures = []string{"ipp-everywhere", "ipp-everywhere-server"}
	}
	model.Attributes = synthesizePrinterAttributes(model)
	return model
}

func policyConfigEmpty(policy config.PolicyConfig) bool {
	return strings.TrimSpace(policy.Media) == "" && len(policy.MediaSupported) == 0 &&
		strings.TrimSpace(policy.MediaDefault) == "" && strings.TrimSpace(policy.MediaType) == "" &&
		strings.TrimSpace(policy.PrintColorMode) == "" && policy.MediaSource == nil &&
		policy.PrintScaling == nil && !policy.UseMediaCol
}

func uniqueOperations(operations []goipp.Op) []goipp.Op {
	seen := make(map[goipp.Op]struct{}, len(operations))
	result := make([]goipp.Op, 0, len(operations))
	for _, operation := range operations {
		if _, found := seen[operation]; found {
			continue
		}
		seen[operation] = struct{}{}
		result = append(result, operation)
	}
	return result
}

func missingOperations(operations, required []goipp.Op) []goipp.Op {
	available := make(map[goipp.Op]struct{}, len(operations))
	for _, operation := range operations {
		available[operation] = struct{}{}
	}
	missing := make([]goipp.Op, 0)
	for _, operation := range required {
		if _, found := available[operation]; !found {
			missing = append(missing, operation)
		}
	}
	return missing
}

// FilterPrinterAttributesWithModel returns a deep copy of the synthesized
// attribute snapshot held by model.
func FilterPrinterAttributesWithModel(model CapabilityModel) goipp.Attributes {
	if model.Attributes == nil {
		model = SynthesizeCapabilityModel(model)
	}
	return model.Attributes.DeepCopy()
}

// FilterPrinterAttributesWithOperations is the dynamic-operation counterpart
// to the historical FilterPrinterAttributes helper.
func FilterPrinterAttributesWithOperations(upstream goipp.Attributes, queueName, proxyURI string, printer config.PrinterConfig, operations []goipp.Op, metadata ...PrinterIdentityMetadata) goipp.Attributes {
	disabled := printer.IPPEverywhereMode == config.IPPEverywhereDisabled || !upstreamClaimsIPPEverywhere(upstream)
	var operationCopy []goipp.Op
	if operations != nil {
		operationCopy = append([]goipp.Op{}, operations...)
	}
	model := CapabilityModel{
		Upstream: upstream.DeepCopy(), Printer: printer, Policy: printer.Policy,
		QueueName: queueName, ProxyURI: proxyURI, Disabled: disabled,
		Operations: operationCopy,
	}
	if len(metadata) > 0 {
		model.Identity = metadata[0]
	}
	return FilterPrinterAttributesWithModel(SynthesizeCapabilityModel(model))
}

// FilterPrinterAttributes removes upstream policy/identity values and adds
// proxy-owned values. The optional metadata argument preserves compatibility
// for callers that only need policy filtering; Service always supplies its
// process-owned metadata so repeated capability queries share one identity and
// one configuration-change epoch.
func FilterPrinterAttributes(upstream goipp.Attributes, queueName, proxyURI string, printer config.PrinterConfig, metadata ...PrinterIdentityMetadata) goipp.Attributes {
	return FilterPrinterAttributesWithOperations(upstream, queueName, proxyURI, printer, nil, metadata...)
}

func synthesizePrinterAttributes(model CapabilityModel) goipp.Attributes {
	upstream := model.Upstream
	queueName := model.QueueName
	proxyURI := model.ProxyURI
	printer := model.Printer
	out := filteredPassthroughPrinterAttributes(upstream)
	identity := defaultPrinterIdentityMetadata(proxyURI)
	if model.Identity.UUID != "" || model.Identity.Uptime != 0 || model.Identity.ConfigChangeTime != 0 || !model.Identity.ConfigChangeDateTime.IsZero() {
		identity = model.Identity
	}
	if identity.UUID == "" {
		identity.UUID = proxyPrinterUUID(proxyURI)
	}
	if identity.ConfigChangeDateTime.IsZero() {
		identity.ConfigChangeDateTime = defaultPrinterIdentityMetadata(proxyURI).ConfigChangeDateTime
	}
	// PrinterIdentityMetadata is optional for compatibility callers and may
	// therefore contain only a UUID or date. IPP declares both clock values as
	// integer(1:MAX); never let an omitted or stale zero value reach the client.
	if identity.Uptime < 1 {
		identity.Uptime = 1
	}
	if identity.ConfigChangeTime < 1 {
		identity.ConfigChangeTime = 1
	}
	if identity.ConfigChangeTime > identity.Uptime {
		identity.ConfigChangeTime = identity.Uptime
	}
	color := strings.EqualFold(strings.TrimSpace(printer.Policy.PrintColorMode), "color")
	outputMode := "monochrome"
	if color {
		outputMode = "color"
	}
	out = append(out,
		iattr.URI("printer-uri-supported", proxyURI),
		iattr.Name("printer-name", printer.DisplayName),
		iattr.Text("printer-info", fmt.Sprintf("%s via policy proxy", printer.DisplayName)),
		iattr.Text("printer-location", printer.Location),
	)
	security := "none"
	if strings.HasPrefix(strings.ToLower(proxyURI), "ipps://") {
		security = "tls"
	}
	for _, owned := range (goipp.Attributes{
		iattr.Keywords("ipp-versions-supported", "1.0", "1.1", "2.0"),
		iattr.Keyword("uri-authentication-supported", "none"),
		iattr.Keyword("uri-security-supported", security),
		goipp.MakeAttribute("charset-configured", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("charset-supported", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("natural-language-configured", goipp.TagLanguage, goipp.String("en")),
		goipp.MakeAttribute("generated-natural-language-supported", goipp.TagLanguage, goipp.String("en")),
	}) {
		out = iattr.SetAttr(out, owned)
	}
	if _, ok := iattr.Attr(out, "printer-is-accepting-jobs"); !ok {
		out = append(out, iattr.Boolean("printer-is-accepting-jobs", true))
	}
	if _, ok := iattr.Attr(out, "printer-state"); !ok {
		out = append(out, goipp.MakeAttribute("printer-state", goipp.TagEnum, goipp.Integer(3)))
	}
	if _, ok := iattr.Attr(out, "printer-state-reasons"); !ok {
		out = append(out, iattr.Keyword("printer-state-reasons", "none"))
	}
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
		iattr.Boolean("color-supported", color),
		iattr.Keyword("output-mode-supported", outputMode),
		iattr.Keyword("output-mode-default", outputMode),
		operationsSupportedAttrFor(model.Operations),
		iattr.Boolean("multiple-document-jobs-supported", false),
	)
	if hasMediaSizeSupported {
		out = append(out, mediaSizeSupported)
	}
	// A nil media-source policy may advertise a source only when the upstream
	// proves that automatic selection (or its own default) is valid. Leaking a
	// made-up "auto" keyword or a stale tray name creates a capability that
	// cannot be selected by the printer.
	if advertisedMediaSource := mediaSource; advertisedMediaSource == "" {
		advertisedMediaSource = provenAutomaticMediaSource(upstream)
		if advertisedMediaSource != "" {
			out = append(out,
				iattr.Keyword("media-source-supported", advertisedMediaSource),
				iattr.Keyword("media-source-default", advertisedMediaSource),
			)
		}
	} else {
		out = append(out,
			iattr.Keyword("media-source-supported", advertisedMediaSource),
			iattr.Keyword("media-source-default", advertisedMediaSource),
		)
	}
	out = append(out, filteredReadyAttributes(upstream, catalog, printer.Policy)...)
	out = append(out, synthesizedFormatAttributes(upstream, printer.Policy, model.IPPFeatures, model.IPPEligible)...)
	if len(model.IPPFeatures) > 0 {
		out = append(out, iattr.Keywords("ipp-features-supported", model.IPPFeatures...))
	}
	_ = queueName
	return out
}

var safePassthroughPrinterAttrs = map[string]struct{}{
	"charset-configured": {}, "charset-supported": {},
	"natural-language-configured": {}, "generated-natural-language-supported": {},
	"compression-supported": {}, "copies-default": {}, "copies-supported": {},
	"finishings-default": {}, "finishings-supported": {},
	"orientation-requested-default": {}, "orientation-requested-supported": {},
	"output-bin-default": {}, "output-bin-supported": {},
	"page-ranges-supported": {}, "number-up-default": {}, "number-up-supported": {},
	"presentation-direction-number-up-default": {}, "presentation-direction-number-up-supported": {},
	"print-quality-default": {}, "print-quality-supported": {},
	"print-scaling-default": {}, "print-scaling-supported": {},
	"printer-kind": {}, "printer-make-and-model": {}, "printer-more-info": {}, "printer-icons": {},
	"printer-state": {}, "printer-state-reasons": {}, "printer-is-accepting-jobs": {},
	"sides-default": {}, "sides-supported": {},
	"printer-resolution-default": {}, "printer-resolution-supported": {},
	"pdf-versions-supported": {}, "jpeg-features-supported": {},
	"jpeg-k-octets-supported": {}, "jpeg-x-dimension-supported": {}, "jpeg-y-dimension-supported": {},
	"job-creation-attributes-supported": {},
}

func filteredPassthroughPrinterAttributes(upstream goipp.Attributes) goipp.Attributes {
	result := make(goipp.Attributes, 0, len(upstream))
	for _, attr := range upstream {
		name := strings.ToLower(strings.TrimSpace(attr.Name))
		if _, constrained := constrainedPrinterAttrSet[name]; constrained {
			continue
		}
		if _, safe := safePassthroughPrinterAttrs[name]; !safe {
			continue
		}
		result = append(result, attr.DeepCopy())
	}
	return result
}

var constrainedPrinterAttrSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(constrainedPrinterAttrs)+8)
	for _, name := range constrainedPrinterAttrs {
		set[strings.ToLower(name)] = struct{}{}
	}
	for _, name := range []string{
		"document-format-supported", "document-format-default", "document-format-preferred",
		"ipp-features-supported", "pwg-raster-document-resolution-supported", "pwg-raster-document-sheet-back",
	} {
		set[name] = struct{}{}
	}
	return set
}()

// URF capabilities are a comma-separated set of family tokens. The token
// prefixes below are the forms used by the AirPrint/URF capability grammar;
// values after a prefix are either a decimal value or a hyphen-separated
// list of decimal values. Resolution values are special: each member must be
// strictly positive because RS describes a print resolution.
var (
	urfVersionToken             = regexp.MustCompile(`(?i)^V[0-9]+\.[0-9]+$`)
	urfWidthToken               = regexp.MustCompile(`(?i)^W[1-9][0-9]*(?:-[1-9][0-9]*)*$`)
	urfColorSpaceToken          = regexp.MustCompile(`(?i)^(?:SRGB|ADOBERGB|DEVRGB|DEVW|DEVCMYK)[1-9][0-9]*(?:-[1-9][0-9]*)*$`)
	urfPositiveNumericListToken = regexp.MustCompile(`(?i)^(?:CP|DM|FN|IS|MT|OB|PQ)[1-9][0-9]*(?:-[1-9][0-9]*)*$`)
	urfOptionalZeroToken        = regexp.MustCompile(`(?i)^(?:IFU|OFU)(?:0|[1-9][0-9]*)$`)
	urfResolutionToken          = regexp.MustCompile(`(?i)^RS[1-9][0-9]*(?:-[1-9][0-9]*)*$`)
	pwgBlackRasterType          = regexp.MustCompile(`(?i)^black_(?:1|8|16)$`)
	pwgGrayRasterType           = regexp.MustCompile(`(?i)^sgray_(?:1|8|16)$`)
	pwgColorRasterType          = regexp.MustCompile(`(?i)^(?:srgb|rgb|adobe-rgb|cmyk)_(?:8|16)$`)
	pwgDeviceRasterType         = regexp.MustCompile(`(?i)^device(?:[1-9]|1[0-5])_(?:8|16)$`)
)

func synthesizedFormatAttributes(upstream goipp.Attributes, policy config.PolicyConfig, _ []string, _ bool) goipp.Attributes {
	formatAttr, hasFormats := iattr.Attr(upstream, "document-format-supported")
	if !hasFormats {
		return nil
	}
	pwgOK := validPWGRasterFamily(upstream, policy)
	urfOK := validURFFamily(upstream)
	values := make(goipp.Values, 0, len(formatAttr.Values))
	for _, value := range formatAttr.Values {
		format, ok := value.V.(goipp.String)
		if !ok || strings.TrimSpace(string(format)) == "" {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(string(format)))
		switch lower {
		case "image/pwg-raster":
			if !pwgOK {
				continue
			}
		case "image/urf":
			if !urfOK {
				continue
			}
		}
		values.Add(goipp.TagMimeType, goipp.String(format))
	}
	if len(values) == 0 {
		return nil
	}
	result := goipp.Attributes{{Name: "document-format-supported", Values: values}}
	if attr, ok := iattr.Attr(upstream, "document-format-default"); ok {
		if format, valid := fixedStringValue(attr.Values); valid && formatInValues(format, values) {
			result = append(result, goipp.MakeAttribute("document-format-default", goipp.TagMimeType, goipp.String(format)))
		} else {
			result = append(result, goipp.MakeAttribute("document-format-default", goipp.TagMimeType, values[0].V))
		}
	}
	if attr, ok := iattr.Attr(upstream, "document-format-preferred"); ok {
		if format, valid := fixedStringValue(attr.Values); valid && formatInValues(format, values) {
			result = append(result, goipp.MakeAttribute("document-format-preferred", goipp.TagMimeType, goipp.String(format)))
		}
	}
	if pwgOK {
		if attr, ok := iattr.Attr(upstream, "pwg-raster-document-type-supported"); ok {
			if filtered, valid := filteredPWGRasterTypes(attr, policy.PrintColorMode); valid {
				result = append(result, filtered)
			}
		}
		if attr, ok := iattr.Attr(upstream, "pwg-raster-document-resolution-supported"); ok {
			result = append(result, attr.DeepCopy())
		}
		if attr, ok := iattr.Attr(upstream, "pwg-raster-document-sheet-back"); ok {
			result = append(result, attr.DeepCopy())
		}
	}
	if urfOK {
		if attr, ok := iattr.Attr(upstream, "urf-supported"); ok {
			result = append(result, attr.DeepCopy())
		}
	}
	return result
}

func fixedStringValue(values goipp.Values) (string, bool) {
	if len(values) != 1 || values[0].V == nil {
		return "", false
	}
	value, ok := values[0].V.(goipp.String)
	return strings.TrimSpace(string(value)), ok && strings.TrimSpace(string(value)) != ""
}

func formatInValues(format string, values goipp.Values) bool {
	for _, value := range values {
		if candidate, ok := value.V.(goipp.String); ok && strings.EqualFold(string(candidate), format) {
			return true
		}
	}
	return false
}

func validURFFamily(upstream goipp.Attributes) bool {
	formats, ok := iattr.Attr(upstream, "document-format-supported")
	if !ok || !hasString(formats, "image/urf") {
		return false
	}
	attr, ok := iattr.Attr(upstream, "urf-supported")
	if !ok || len(attr.Values) == 0 {
		return false
	}
	width := false
	colorSpace := false
	resolution := false
	tokens := 0
	for _, value := range attr.Values {
		if value.T != goipp.TagKeyword {
			return false
		}
		text, valid := value.V.(goipp.String)
		if !valid {
			return false
		}
		parts, valid := splitURFTokens(string(text))
		if !valid {
			return false
		}
		for _, token := range parts {
			tokens++
			switch {
			case urfResolutionToken.MatchString(token):
				resolution = true
			case urfWidthToken.MatchString(token):
				width = true
			case urfColorSpaceToken.MatchString(token):
				colorSpace = true
			case urfVersionToken.MatchString(token), urfPositiveNumericListToken.MatchString(token), urfOptionalZeroToken.MatchString(token):
			default:
				return false
			}
		}
	}
	return tokens >= 3 && width && colorSpace && resolution
}

// splitURFTokens accepts both the comma-separated representation used by
// DNS-SD and the separate keyword values used by IPP, while rejecting empty
// comma members and a trailing comma. Whitespace around a comma is harmless;
// a comma is still required to delimit an empty member, so malformed values
// cannot be silently discarded by strings.FieldsFunc.
func splitURFTokens(text string) ([]string, bool) {
	var tokens []string
	start := -1
	commaPending := false
	for index, r := range text {
		if r == ',' || unicode.IsSpace(r) {
			if start >= 0 {
				tokens = append(tokens, text[start:index])
				start = -1
				commaPending = false
			}
			if r == ',' {
				if len(tokens) == 0 || commaPending {
					return nil, false
				}
				commaPending = true
			}
			continue
		}
		if start < 0 {
			start = index
		}
		commaPending = false
	}
	if start >= 0 {
		tokens = append(tokens, text[start:])
	}
	if commaPending || len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
}

func validPWGRasterFamily(upstream goipp.Attributes, policy config.PolicyConfig) bool {
	formats, ok := iattr.Attr(upstream, "document-format-supported")
	if !ok || !hasString(formats, "image/pwg-raster") {
		return false
	}
	types, ok := iattr.Attr(upstream, "pwg-raster-document-type-supported")
	if !ok || len(types.Values) == 0 {
		return false
	}
	if _, valid := filteredPWGRasterTypes(types, policy.PrintColorMode); !valid {
		return false
	}
	resolutions, ok := iattr.Attr(upstream, "pwg-raster-document-resolution-supported")
	if !ok || len(resolutions.Values) == 0 {
		return false
	}
	for _, value := range resolutions.Values {
		if value.T != goipp.TagResolution {
			return false
		}
		resolution, valid := value.V.(goipp.Resolution)
		if !valid || resolution.Xres <= 0 || resolution.Yres <= 0 || (resolution.Units != goipp.UnitsDpi && resolution.Units != goipp.UnitsDpcm) {
			return false
		}
	}
	sheetBack, ok := iattr.Attr(upstream, "pwg-raster-document-sheet-back")
	if !ok || len(sheetBack.Values) != 1 || sheetBack.Values[0].T != goipp.TagKeyword {
		return false
	}
	sheet, valid := fixedKeyword(sheetBack.Values)
	return valid && (strings.EqualFold(sheet, "normal") || strings.EqualFold(sheet, "flipped") || strings.EqualFold(sheet, "rotated") || strings.EqualFold(sheet, "manual-tumble"))
}

func filteredPWGRasterTypes(attr goipp.Attribute, colorMode string) (goipp.Attribute, bool) {
	if len(attr.Values) == 0 {
		return goipp.Attribute{}, false
	}
	values := make(goipp.Values, 0, len(attr.Values))
	for _, value := range attr.Values {
		if value.T != goipp.TagKeyword {
			return goipp.Attribute{}, false
		}
		text, valid := value.V.(goipp.String)
		if !valid || strings.TrimSpace(string(text)) == "" {
			return goipp.Attribute{}, false
		}
		if !validPWGRasterType(string(text)) {
			return goipp.Attribute{}, false
		}
		if rasterTypeCompatible(string(text), colorMode) {
			values.Add(value.T, value.V.DeepCopy())
		}
	}
	if len(values) == 0 {
		return goipp.Attribute{}, false
	}
	return goipp.Attribute{Name: attr.Name, Values: values}, true
}

func rasterTypeCompatible(rasterType, colorMode string) bool {
	value := strings.ToLower(strings.TrimSpace(rasterType))
	if !validPWGRasterType(value) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(colorMode), "monochrome") {
		return pwgBlackRasterType.MatchString(value) || pwgGrayRasterType.MatchString(value)
	}
	if strings.EqualFold(strings.TrimSpace(colorMode), "color") {
		return pwgColorRasterType.MatchString(value) || pwgDeviceRasterType.MatchString(value)
	}
	return true
}

func validPWGRasterType(rasterType string) bool {
	value := strings.TrimSpace(rasterType)
	return pwgBlackRasterType.MatchString(value) ||
		pwgGrayRasterType.MatchString(value) ||
		pwgColorRasterType.MatchString(value) ||
		pwgDeviceRasterType.MatchString(value)
}

func policyMediaSource(policy config.PolicyConfig) string {
	if policy.MediaSource == nil {
		return ""
	}
	return strings.TrimSpace(*policy.MediaSource)
}

// provenAutomaticMediaSource selects the source a policy may safely use when
// it does not pin a tray. "auto" is preferred only when the upstream actually
// advertises it. Otherwise the upstream's advertised default is retained. A
// missing or malformed pair means that no source should be invented.
func provenAutomaticMediaSource(upstream goipp.Attributes) string {
	if attr, ok := iattr.Attr(upstream, "media-source-supported"); ok {
		for _, value := range attr.Values {
			if source, valid := fixedKeyword(goipp.Values{value}); valid && strings.EqualFold(source, "auto") {
				return source
			}
		}
	}
	if attr, ok := iattr.Attr(upstream, "media-source-default"); ok {
		if source, valid := fixedKeyword(attr.Values); valid {
			if supported, ok := iattr.Attr(upstream, "media-source-supported"); ok && len(supported.Values) > 0 {
				if hasString(supported, source) {
					return source
				}
				return ""
			}
			return source
		}
	}
	return ""
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
	hasType := strings.TrimSpace(policy.MediaType) != ""
	hasSource := policyMediaSource(policy) != ""
	hasBottom, hasLeft, hasRight, hasTop := false, false, false, false
	for _, size := range catalog.Instances {
		hasType = hasType || size.HasMediaType
		hasSource = hasSource || size.HasMediaSource
		hasBottom = hasBottom || size.HasBottomMargin
		hasLeft = hasLeft || size.HasLeftMargin
		hasRight = hasRight || size.HasRightMargin
		hasTop = hasTop || size.HasTopMargin
	}
	if hasType {
		result = append(result, "media-type")
	}
	if hasBottom {
		result = append(result, "media-bottom-margin")
	}
	if hasLeft {
		result = append(result, "media-left-margin")
	}
	if hasRight {
		result = append(result, "media-right-margin")
	}
	if hasTop {
		result = append(result, "media-top-margin")
	}
	if hasSource {
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
	if mediaType == "" && size.HasMediaType {
		mediaType = size.MediaType
	}
	if mediaType != "" {
		members = append(members, iattr.Keyword("media-type", mediaType))
	}
	if mediaSource == "" && size.HasMediaSource {
		mediaSource = size.MediaSource
	}
	if mediaSource != "" {
		members = append(members, iattr.Keyword("media-source", mediaSource))
	}
	members = appendKnownMargins(members, size)
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, goipp.Collection(members))
}

func appendKnownMargins(members goipp.Attributes, size mediaSize) goipp.Attributes {
	if size.HasBottomMargin {
		members = append(members, iattr.Integer("media-bottom-margin", size.BottomMargin))
	}
	if size.HasLeftMargin {
		members = append(members, iattr.Integer("media-left-margin", size.LeftMargin))
	}
	if size.HasRightMargin {
		members = append(members, iattr.Integer("media-right-margin", size.RightMargin))
	}
	if size.HasTopMargin {
		members = append(members, iattr.Integer("media-top-margin", size.TopMargin))
	}
	return members
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
	upstreamReady := make(map[string]struct{})
	if attr, ok := iattr.Attr(upstream, "media-ready"); ok {
		if values, valid := attributeStrings(attr); valid {
			for _, value := range values {
				upstreamReady[mediaNameKey(value)] = struct{}{}
			}
		}
	}

	// Resolve media-col-ready first because it carries instance identity. A
	// name-only media-ready response is handled below using the same preferred
	// instance selected for media-col-default.
	readySizes := make([]mediaSize, 0, len(catalog.Instances))
	if attr, ok := iattr.Attr(upstream, "media-col-ready"); ok {
		for _, value := range attr.Values {
			collection, valid := mediaCollectionFromValue(value.V)
			if !valid {
				continue
			}
			index := catalogIndexForCollection(collection, catalog)
			if index < 0 {
				continue
			}
			resolved := catalog.Instances[index]
			overlayMediaSize(&resolved, collection.size)
			resolved.Ready = true
			if containsReadyInstance(readySizes, resolved) {
				continue
			}
			readySizes = append(readySizes, resolved)
		}
	}

	for key := range upstreamReady {
		if hasReadyName(readySizes, key) {
			continue
		}
		if size, ok := catalog.ByName[key]; ok {
			size.Ready = true
			readySizes = append(readySizes, size)
		}
	}

	if len(readySizes) > 0 {
		readyNames := make([]string, 0, len(readySizes))
		seenNames := make(map[string]struct{}, len(readySizes))
		for _, size := range readySizes {
			key := mediaNameKey(size.Name)
			if _, seen := seenNames[key]; seen {
				continue
			}
			seenNames[key] = struct{}{}
			readyNames = append(readyNames, size.Name)
		}
		if len(readyNames) > 0 {
			result = append(result, iattr.Keywords("media-ready", readyNames...))
		}
		result = append(result, mediaColReadyAttr(readySizes, policy.MediaType, policyMediaSource(policy)))
	}
	return result
}

func containsReadyInstance(sizes []mediaSize, wanted mediaSize) bool {
	for _, size := range sizes {
		if sameMediaInstance(size, wanted) && dimensionsEqual(size, wanted) {
			return true
		}
	}
	return false
}

func hasReadyName(sizes []mediaSize, name string) bool {
	for _, size := range sizes {
		if mediaNameKey(size.Name) == name {
			return true
		}
	}
	return false
}

func catalogIndexForCollection(collection mediaCollection, catalog mediaCatalog) int {
	bestIndex := -1
	bestScore := -1
	for index, size := range catalog.Instances {
		score := 0
		nameMatch := false
		for _, alias := range collection.names {
			if strings.EqualFold(strings.TrimSpace(alias), strings.TrimSpace(size.Name)) {
				nameMatch = true
				score += 4
				break
			}
		}
		if collection.size.Name != "" && strings.EqualFold(strings.TrimSpace(collection.size.Name), strings.TrimSpace(size.Name)) {
			nameMatch = true
			score += 4
		}
		if !nameMatch && collection.size.HasDimension && dimensionsEqual(collection.size, size) {
			score += 2
		}
		if !nameMatch && score == 0 {
			continue
		}
		if collection.size.HasMediaSource {
			if !size.HasMediaSource || !strings.EqualFold(collection.size.MediaSource, size.MediaSource) {
				continue
			}
			score += 3
		}
		if collection.size.HasMediaType {
			if !size.HasMediaType || !strings.EqualFold(collection.size.MediaType, size.MediaType) {
				continue
			}
			score += 3
		}
		if collection.size.HasDimension {
			if !size.HasDimension || !dimensionsEqual(collection.size, size) {
				continue
			}
			score += 2
		}
		if score > bestScore {
			bestIndex, bestScore = index, score
		}
	}
	return bestIndex
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
	if src.HasMediaType {
		dst.MediaType, dst.HasMediaType = src.MediaType, true
	}
	if src.HasMediaSource {
		dst.MediaSource, dst.HasMediaSource = src.MediaSource, true
	}
	dst.Ready = dst.Ready || src.Ready
}

func operationsSupportedAttr() goipp.Attribute {
	return operationsSupportedAttrFor(proxyOperations)
}

func operationsSupportedAttrFor(ops []goipp.Op) goipp.Attribute {
	if len(ops) == 0 {
		// Get-Printer-Attributes remains the proxy's read-only operation even
		// for a deactivated/empty dynamic operation surface. IPP requires
		// operations-supported to be a non-empty 1setOf enum; an empty
		// attribute is malformed and cannot describe that state.
		ops = []goipp.Op{goipp.OpGetPrinterAttributes}
	}
	values := make([]goipp.Value, len(ops))
	for i, op := range ops {
		values[i] = goipp.Integer(op)
	}
	return goipp.MakeAttr("operations-supported", goipp.TagEnum, values[0], values[1:]...)
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
