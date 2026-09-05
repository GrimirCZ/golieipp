package proxy

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

var policyDropAttrs = []string{
	"media",
	"media-col",
	"media-type",
	"media-source",
	"media-weight-metric",
	"media-color",
	"media-pre-printed",
	"media-hole-count",
	"media-order-count",
	"media-recycled",
	"media-tooth",
	"print-color-mode",
	"output-mode",
}

type NormalizationLog struct {
	ClientMedia          string
	ClientMediaCol       string
	ClientMediaType      string
	ClientMediaSource    string
	ClientPrintColorMode string
	RequestedMedia       string
	RequestedMediaCol    string
	EffectiveMedia       string
	MediaFallback        bool
	ForcedMedia          string
	ForcedMediaType      string
	ForcedMediaSource    string
	ForcedPrintColorMode string
}

type FidelityMode string

const (
	FidelityWarn   FidelityMode = "warn"
	FidelityReject FidelityMode = "reject"
)

type NormalizationOptions struct {
	Policy           config.PolicyConfig
	Upstream         goipp.Attributes
	DropVendorAttrs  []string
	PreserveJobAttrs []string
	Fidelity         bool
	FidelityMode     FidelityMode
}

type NormalizationResult struct {
	Attrs         goipp.Attributes
	Log           NormalizationLog
	Unsupported   goipp.Attributes
	Substitutions []string
	Substituted   bool
}

type NormalizationError struct {
	Status      goipp.Status
	Message     string
	Unsupported goipp.Attributes
}

func (e *NormalizationError) Error() string { return e.Message }

// NormalizeJobAttrsWithOptions is the policy boundary used by the protocol
// processor. It distinguishes invalid syntax from a valid value that policy
// substitutes and applies preserve_job_attrs as an authoritative allowlist.
func NormalizeJobAttrsWithOptions(attrs goipp.Attributes, options NormalizationOptions) (NormalizationResult, error) {
	if options.FidelityMode == "" {
		options.FidelityMode = FidelityWarn
	}
	if options.FidelityMode != FidelityWarn && options.FidelityMode != FidelityReject {
		return NormalizationResult{}, &NormalizationError{Status: goipp.StatusErrorBadRequest, Message: "invalid fidelity mode"}
	}
	if invalid := malformedPolicyAttributes(attrs); len(invalid) > 0 {
		return NormalizationResult{}, &NormalizationError{Status: goipp.StatusErrorAttributesOrValues, Message: "malformed policy-controlled attribute", Unsupported: invalid}
	}

	preserve := make(map[string]struct{}, len(options.PreserveJobAttrs))
	for _, name := range options.PreserveJobAttrs {
		preserve[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	policyNames := make(map[string]struct{}, len(policyDropAttrs)+len(options.DropVendorAttrs))
	for _, name := range policyDropAttrs {
		policyNames[strings.ToLower(name)] = struct{}{}
	}
	for _, name := range options.DropVendorAttrs {
		policyNames[strings.ToLower(name)] = struct{}{}
	}
	input := make(goipp.Attributes, 0, len(attrs))
	var unsupported goipp.Attributes
	for _, attr := range attrs {
		name := strings.ToLower(attr.Name)
		if _, controlled := policyNames[name]; controlled {
			input = append(input, attr)
			continue
		}
		if _, allowed := preserve[name]; allowed {
			input = append(input, attr)
			continue
		}
		unsupported = append(unsupported, attr)
	}
	if options.Fidelity && len(unsupported) > 0 {
		return NormalizationResult{}, &NormalizationError{Status: goipp.StatusErrorAttributesOrValues, Message: "unsupported job-template attributes", Unsupported: unsupported}
	}

	normalized, log := NormalizeJobAttrsWithCapabilities(input, options.Policy, options.DropVendorAttrs, options.Upstream)
	result := NormalizationResult{Attrs: normalized, Log: log, Unsupported: unsupported, Substituted: len(unsupported) > 0}
	for _, attr := range unsupported {
		result.Substitutions = append(result.Substitutions, attr.Name)
	}
	addSubstitution := func(name string, attr goipp.Attribute, present bool) {
		if !present {
			return
		}
		result.Substituted = true
		result.Substitutions = append(result.Substitutions, name)
		result.Unsupported = append(result.Unsupported, attr)
	}
	if attr, present := iattr.Attr(attrs, "media"); present {
		addSubstitution("media", attr, log.MediaFallback || !iattr.HasStringValue(normalized, "media", log.ClientMedia))
	}
	if attr, present := iattr.Attr(attrs, "media-col"); present {
		addSubstitution("media-col", attr, log.MediaFallback || !options.Policy.UseMediaCol)
	}
	for _, rule := range []struct {
		name string
		want string
	}{
		{"media-type", options.Policy.MediaType},
		{"media-source", policyMediaSource(options.Policy)},
		{"print-color-mode", options.Policy.PrintColorMode},
		{"output-mode", options.Policy.PrintColorMode},
	} {
		if attr, present := iattr.Attr(attrs, rule.name); present {
			got, _ := iattr.FirstString(attrs, rule.name)
			addSubstitution(rule.name, attr, !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(rule.want)))
		}
	}
	for _, name := range options.DropVendorAttrs {
		if attr, present := iattr.Attr(attrs, name); present {
			addSubstitution(attr.Name, attr, true)
		}
	}
	if options.Fidelity && options.FidelityMode == FidelityReject && result.Substituted {
		return NormalizationResult{}, &NormalizationError{Status: goipp.StatusErrorAttributesOrValues, Message: "job attributes conflict with queue policy", Unsupported: result.Unsupported}
	}
	return result, nil
}

func malformedPolicyAttributes(attrs goipp.Attributes) goipp.Attributes {
	var invalid goipp.Attributes
	for _, attr := range attrs {
		name := strings.ToLower(attr.Name)
		switch name {
		case "media", "media-type", "media-source", "print-color-mode", "output-mode":
			if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagKeyword {
				invalid = append(invalid, attr)
				continue
			}
			value, ok := attr.Values[0].V.(goipp.String)
			if !ok || strings.TrimSpace(string(value)) == "" {
				invalid = append(invalid, attr)
			}
		case "media-col":
			if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagBeginCollection || !validMediaColSyntax(attr) {
				invalid = append(invalid, attr)
			}
		}
	}
	return invalid
}

// NormalizeJobAttrs keeps the original API and resolves standard PWG media
// names without requiring a prior capability probe. Service request paths use
// NormalizeJobAttrsWithCapabilities so custom printer media names can also be
// matched by their upstream media-col-database entries.
func NormalizeJobAttrs(attrs goipp.Attributes, policy config.PolicyConfig, vendorDrop []string) (goipp.Attributes, NormalizationLog) {
	return NormalizeJobAttrsWithCapabilities(attrs, policy, vendorDrop, nil)
}

func NormalizeJobAttrsWithCapabilities(attrs goipp.Attributes, policy config.PolicyConfig, vendorDrop []string, upstream goipp.Attributes) (goipp.Attributes, NormalizationLog) {
	catalog := buildMediaCatalog(upstream, policy)
	selected, requested, requestedCol, fallback := selectRequestedMedia(attrs, catalog, policy)
	if selected.Name == "" {
		selected = catalog.Sizes[0]
		fallback = true
	}

	log := NormalizationLog{
		ForcedMedia:          selected.Name,
		ForcedMediaType:      policy.MediaType,
		ForcedPrintColorMode: policy.PrintColorMode,
		RequestedMedia:       requested,
		RequestedMediaCol:    requestedCol,
		EffectiveMedia:       selected.Name,
		MediaFallback:        fallback,
	}
	if source := policyMediaSource(policy); source != "" {
		log.ForcedMediaSource = source
	}
	log.ClientMedia, _ = iattr.FirstString(attrs, "media")
	if attr, ok := iattr.Attr(attrs, "media-col"); ok {
		log.ClientMediaCol = attributeDisplay(attr)
	}
	log.ClientMediaType, _ = iattr.FirstString(attrs, "media-type")
	log.ClientMediaSource, _ = iattr.FirstString(attrs, "media-source")
	log.ClientPrintColorMode, _ = iattr.FirstString(attrs, "print-color-mode")

	drops := append([]string{}, policyDropAttrs...)
	drops = append(drops, vendorDrop...)
	out := iattr.DropAttrs(attrs, drops...)
	out = sanitizeOverrides(out, vendorDrop)

	if policy.UseMediaCol {
		out = append(out, mediaColJobAttr("media-col", selected, policy.MediaType, policyMediaSource(policy)))
	} else {
		out = append(out, iattr.Keyword("media", selected.Name))
		if policy.MediaType != "" {
			out = append(out, iattr.Keyword("media-type", policy.MediaType))
		}
	}
	if !policy.UseMediaCol && policyMediaSource(policy) != "" {
		out = append(out, iattr.Keyword("media-source", policyMediaSource(policy)))
	}
	if policy.PrintScaling != nil && *policy.PrintScaling != "" {
		out = iattr.SetAttr(out, iattr.Keyword("print-scaling", *policy.PrintScaling))
	}
	out = append(out, iattr.Keyword("print-color-mode", policy.PrintColorMode))
	if policy.PrintColorMode == "monochrome" {
		out = append(out, iattr.Keyword("output-mode", "monochrome"))
	}
	return out, log
}

// sanitizeOverrides removes policy-controlled job-template attributes from
// each overrides collection before it is sent upstream. An overrides
// collection is a nested 1setOf collection, so dropping only top-level media
// and media-col would let a client bypass the proxy's fixed media policy on a
// later document or page. Non-policy attributes are retained and malformed or
// empty collections are discarded.
func sanitizeOverrides(attrs goipp.Attributes, vendorDrop []string) goipp.Attributes {
	result := make(goipp.Attributes, 0, len(attrs))
	for _, attr := range attrs {
		if !strings.EqualFold(attr.Name, "overrides") {
			result = append(result, attr)
			continue
		}
		sanitized, ok := sanitizeOverridesAttr(attr, vendorDrop)
		if ok {
			result = append(result, sanitized)
		}
	}
	return result
}

func sanitizeOverridesAttr(attr goipp.Attribute, vendorDrop []string) (goipp.Attribute, bool) {
	if len(attr.Values) == 0 {
		return goipp.Attribute{}, false
	}

	values := make(goipp.Values, 0, len(attr.Values))
	for _, value := range attr.Values {
		collection, ok := value.V.(goipp.Collection)
		if !ok {
			// A malformed override value cannot be made safe or meaningful, so
			// leave it out while retaining any valid sibling collections.
			continue
		}
		filtered := dropNestedPolicyAttrs(goipp.Attributes(collection), vendorDrop)
		if len(filtered) == 0 || !hasOverridePayload(filtered) {
			continue
		}
		values.Add(goipp.TagBeginCollection, goipp.Collection(filtered))
	}
	if len(values) == 0 {
		return goipp.Attribute{}, false
	}
	return goipp.Attribute{Name: attr.Name, Values: values}, true
}

// hasOverridePayload distinguishes an override that still changes a job from
// a selector-only collection. document-number(s), page-ranges/pages, and
// document-copies identify the target, but are not themselves an override;
// after all policy-controlled members are removed, forwarding such a
// collection would be a meaningless no-op.
func hasOverridePayload(attrs goipp.Attributes) bool {
	for _, attr := range attrs {
		switch strings.ToLower(attr.Name) {
		case "document-number", "document-numbers", "page-ranges", "pages", "document-copies", "copy-numbers":
			continue
		default:
			return true
		}
	}
	return false
}

func dropNestedPolicyAttrs(attrs goipp.Attributes, vendorDrop []string) goipp.Attributes {
	drop := make(map[string]struct{}, len(policyDropAttrs)+len(vendorDrop))
	for _, name := range policyDropAttrs {
		drop[strings.ToLower(name)] = struct{}{}
	}
	for _, name := range vendorDrop {
		drop[strings.ToLower(name)] = struct{}{}
	}

	result := make(goipp.Attributes, 0, len(attrs))
	for _, attr := range attrs {
		if _, ok := drop[strings.ToLower(attr.Name)]; ok {
			continue
		}
		// Only nested collection values are rewritten below. A shallow values
		// clone avoids mutating the caller's attribute slice and also keeps a
		// malformed nil value from panicking DeepCopy.
		copy := attr
		copy.Values = attr.Values.Clone()
		for index, value := range copy.Values {
			if nested, ok := value.V.(goipp.Collection); ok {
				copy.Values[index].V = goipp.Collection(dropNestedPolicyAttrs(goipp.Attributes(nested), vendorDrop))
			}
		}
		result = append(result, copy)
	}
	return result
}

func selectRequestedMedia(attrs goipp.Attributes, catalog mediaCatalog, policy config.PolicyConfig) (mediaSize, string, string, bool) {
	defaultSize := catalog.ByName[mediaNameKey(policyMediaDefault(policy))]
	if defaultSize.Name == "" && len(catalog.Sizes) > 0 {
		defaultSize = catalog.Sizes[0]
	}

	mediaCol, hasMediaCol := iattr.Attr(attrs, "media-col")
	media, hasMedia := iattr.Attr(attrs, "media")
	if hasMediaCol {
		raw := attributeDisplay(mediaCol)
		if selected, ok := resolveMediaCol(mediaCol, catalog); ok {
			return selected, raw, raw, false
		}
		return defaultSize, raw, raw, true
	}
	if hasMedia {
		raw := attributeDisplay(media)
		if values, ok := attributeStrings(media); ok && len(values) == 1 {
			if selected, exists := catalog.ByName[mediaNameKey(values[0])]; exists {
				return selected, raw, "", false
			}
		}
		return defaultSize, raw, "", true
	}
	return defaultSize, "", "", true
}

func attributeStrings(attr goipp.Attribute) ([]string, bool) {
	if len(attr.Values) == 0 {
		return nil, false
	}
	values := make([]string, 0, len(attr.Values))
	for _, value := range attr.Values {
		if value.V == nil {
			return nil, false
		}
		switch typed := value.V.(type) {
		case goipp.String:
			values = append(values, string(typed))
		default:
			return nil, false
		}
	}
	return values, true
}

type mediaColSelector struct {
	Names      []string
	XDimension int
	YDimension int
	HasX       bool
	HasY       bool
	Invalid    bool
}

func resolveMediaCol(attr goipp.Attribute, catalog mediaCatalog) (mediaSize, bool) {
	if len(attr.Values) != 1 {
		return mediaSize{}, false
	}
	collection, ok := attr.Values[0].V.(goipp.Collection)
	if !ok {
		return mediaSize{}, false
	}
	selector := mediaColSelector{}
	collectMediaColSelector(goipp.Attributes(collection), &selector)
	if selector.Invalid {
		return mediaSize{}, false
	}

	var named *mediaSize
	for _, raw := range selector.Names {
		size, exists := catalog.ByName[mediaNameKey(raw)]
		if !exists {
			return mediaSize{}, false
		}
		if named == nil {
			candidate := size
			named = &candidate
		} else if mediaNameKey(named.Name) != mediaNameKey(size.Name) {
			return mediaSize{}, false
		}
	}

	var dimensional *mediaSize
	if selector.HasX || selector.HasY {
		if !selector.HasX || !selector.HasY || selector.XDimension <= 0 || selector.YDimension <= 0 {
			return mediaSize{}, false
		}
		request := mediaSize{XDimension: selector.XDimension, YDimension: selector.YDimension, HasDimension: true}
		for _, size := range catalog.Sizes {
			if !dimensionsEqual(request, size) {
				continue
			}
			if dimensional != nil {
				return mediaSize{}, false
			}
			candidate := size
			dimensional = &candidate
		}
		if dimensional == nil {
			return mediaSize{}, false
		}
	}
	if named != nil && dimensional != nil && !dimensionsEqual(*named, *dimensional) {
		return mediaSize{}, false
	}
	if named != nil {
		return *named, true
	}
	if dimensional != nil {
		return *dimensional, true
	}
	return mediaSize{}, false
}

func collectMediaColSelector(attrs goipp.Attributes, selector *mediaColSelector) {
	for _, attr := range attrs {
		switch strings.ToLower(attr.Name) {
		case "media-key", "media-size-name":
			values, ok := attributeStrings(attr)
			if !ok || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
				selector.Invalid = true
			} else {
				selector.Names = append(selector.Names, strings.TrimSpace(values[0]))
			}
		case "x-dimension":
			value, ok := fixedInteger(attr.Values)
			if !ok || selector.HasX {
				selector.Invalid = true
			} else {
				selector.XDimension, selector.HasX = value, true
			}
		case "y-dimension":
			value, ok := fixedInteger(attr.Values)
			if !ok || selector.HasY {
				selector.Invalid = true
			} else {
				selector.YDimension, selector.HasY = value, true
			}
		}
		for _, value := range attr.Values {
			if nested, ok := value.V.(goipp.Collection); ok {
				collectMediaColSelector(goipp.Attributes(nested), selector)
			}
		}
	}
}

func attributeDisplay(attr goipp.Attribute) string {
	if len(attr.Values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(attr.Values))
	for _, value := range attr.Values {
		if value.V == nil {
			parts = append(parts, "<nil>")
		} else {
			parts = append(parts, fmt.Sprint(value.V))
		}
	}
	return strings.Join(parts, ",")
}

func (n NormalizationLog) Attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("client_media", n.ClientMedia),
		slog.String("client_media_col", n.ClientMediaCol),
		slog.String("client_media_type", n.ClientMediaType),
		slog.String("client_media_source", n.ClientMediaSource),
		slog.String("client_color", n.ClientPrintColorMode),
		slog.String("requested_media", n.RequestedMedia),
		slog.String("requested_media_col", n.RequestedMediaCol),
		slog.String("effective_media", n.EffectiveMedia),
		slog.Bool("media_fallback", n.MediaFallback),
		slog.String("forced_media", n.ForcedMedia),
		slog.String("forced_media_type", n.ForcedMediaType),
		slog.String("forced_media_source", n.ForcedMediaSource),
		slog.String("forced_color", n.ForcedPrintColorMode),
	}
}
