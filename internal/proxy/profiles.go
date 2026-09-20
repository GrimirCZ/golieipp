package proxy

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

// CapabilityProfile is the practical readiness state of one client-facing
// profile. Profiles are intentionally independent: a queue can keep ordinary
// IPP available while AirPrint or IPP Everywhere discovery is withdrawn.
type CapabilityProfile struct {
	Enabled  bool
	Ready    bool
	Reason   string
	Warnings []string
}

// CapabilityProfiles describes the three externally meaningful capabilities
// of a queue. Ordinary IPP is the base service; the other two profiles control
// their DNS-SD subtype and IPP feature advertisements.
type CapabilityProfiles struct {
	Ordinary      CapabilityProfile
	AirPrint      CapabilityProfile
	IPPEverywhere CapabilityProfile
}

func profileReadinessResponseSet(profiles CapabilityProfiles) profileReadinessResponseSetType {
	return profileReadinessResponseSetType{
		Ordinary:      profileResponse(profiles.Ordinary),
		AirPrint:      profileResponse(profiles.AirPrint),
		IPPEverywhere: profileResponse(profiles.IPPEverywhere),
	}
}

// profileReadinessResponseSetType is kept separate from CapabilityProfiles so
// the HTTP shape can remain JSON-focused without making the internal profile
// model depend on encoding tags.
type profileReadinessResponseSetType struct {
	Ordinary      profileReadinessResponse `json:"ordinary"`
	AirPrint      profileReadinessResponse `json:"airprint"`
	IPPEverywhere profileReadinessResponse `json:"ipp_everywhere"`
}

func profileResponse(profile CapabilityProfile) profileReadinessResponse {
	return profileReadinessResponse{
		Enabled:  profile.Enabled,
		Ready:    profile.Ready,
		Reason:   profile.Reason,
		Warnings: append([]string(nil), profile.Warnings...),
	}
}

func (p CapabilityProfiles) Warnings() []string {
	var warnings []string
	appendProfile := func(name string, profile CapabilityProfile) {
		for _, warning := range profile.Warnings {
			warnings = append(warnings, name+": "+warning)
		}
	}
	appendProfile("ordinary", p.Ordinary)
	appendProfile("airprint", p.AirPrint)
	appendProfile("ipp_everywhere", p.IPPEverywhere)
	return warnings
}

func normalizeIPPEverywhereMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", config.IPPEverywhereRequired, config.IPPEverywhereAuto:
		return config.IPPEverywhereAuto
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func normalizeAirPrintMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", config.AirPrintAuto:
		return config.AirPrintAuto
	case config.AirPrintEmulateIfMissing:
		return config.AirPrintEmulateIfMissing
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

// ProjectPolicyAgainstUpstream computes the protected policy snapshot used by
// the forwarding and advertisement paths. Values that are explicitly proven
// unsupported are removed; a missing support attribute is retained with a
// warning because many legacy printers omit optional description attributes.
// A configured dimension that has a present, contradictory support attribute
// is a hard error: silently forwarding an unproven policy would weaken the
// proxy's enforcement contract.
func ProjectPolicyAgainstUpstream(upstream goipp.Attributes, printer config.PrinterConfig) (config.PolicyConfig, []string, error) {
	policy := clonePolicyConfig(printer.Policy)
	var warnings []string

	configuredMedia := policyMediaNames(policy)
	media := make([]string, 0, len(configuredMedia))
	for _, name := range configuredMedia {
		if policyMediaSupportedByUpstream(upstream, policy, name) {
			media = append(media, name)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("policy media %q is not advertised by upstream and was omitted", name))
	}
	if len(media) == 0 {
		return config.PolicyConfig{}, warnings, fmt.Errorf("required policy dimension media has no upstream-supported values")
	}
	policy.MediaSupported = media
	if policy.Media != "" {
		policy.Media = media[0]
	}
	defaultName := strings.TrimSpace(policy.MediaDefault)
	if defaultName == "" {
		defaultName = strings.TrimSpace(policy.Media)
	}
	if !containsFold(media, defaultName) {
		oldDefault := defaultName
		defaultName = media[0]
		warnings = append(warnings, fmt.Sprintf("policy media default %q is not supported; using %q", oldDefault, defaultName))
	}
	policy.MediaDefault = defaultName

	if value := strings.TrimSpace(policy.PrintColorMode); value != "" {
		if present, supported := advertisedKeywordContains(upstream, "print-color-mode-supported", value); present && !supported {
			return config.PolicyConfig{}, warnings, fmt.Errorf("required policy dimension print-color-mode has no upstream-supported value %q", value)
		}
		if !presentAttribute(upstream, "print-color-mode-supported") {
			warnings = append(warnings, "upstream did not advertise print-color-mode-supported; retaining configured policy")
		}
	}

	if value := strings.TrimSpace(policy.MediaType); value != "" {
		if present, supported := advertisedKeywordContains(upstream, "media-type-supported", value); present && !supported {
			return config.PolicyConfig{}, warnings, fmt.Errorf("required policy dimension media-type has no upstream-supported value %q", value)
		}
		if !presentAttribute(upstream, "media-type-supported") {
			warnings = append(warnings, "upstream did not advertise media-type-supported; retaining configured policy")
		}
	}

	if source := policyMediaSource(policy); source != "" {
		if present, supported := advertisedKeywordContains(upstream, "media-source-supported", source); present && !supported {
			return config.PolicyConfig{}, warnings, fmt.Errorf("required policy dimension media-source has no upstream-supported value %q", source)
		}
		if !presentAttribute(upstream, "media-source-supported") {
			warnings = append(warnings, "upstream did not advertise media-source-supported; retaining configured policy")
		}
	}
	return policy, warnings, nil
}

func clonePolicyConfig(policy config.PolicyConfig) config.PolicyConfig {
	copy := policy
	copy.MediaSupported = append([]string(nil), policy.MediaSupported...)
	if policy.MediaSource != nil {
		value := *policy.MediaSource
		copy.MediaSource = &value
	}
	if policy.PrintScaling != nil {
		value := *policy.PrintScaling
		copy.PrintScaling = &value
	}
	return copy
}

func presentAttribute(attrs goipp.Attributes, name string) bool {
	_, ok := iattr.Attr(attrs, name)
	return ok
}

func advertisedKeywordContains(attrs goipp.Attributes, name, wanted string) (present, supported bool) {
	attr, ok := iattr.Attr(attrs, name)
	if !ok {
		return false, false
	}
	if len(attr.Values) == 0 {
		return true, false
	}
	for _, value := range attr.Values {
		if value.T != goipp.TagKeyword {
			return true, false
		}
		text, ok := value.V.(goipp.String)
		if !ok || strings.TrimSpace(string(text)) == "" {
			return true, false
		}
		if strings.EqualFold(strings.TrimSpace(string(text)), wanted) {
			supported = true
		}
	}
	return true, supported
}

func effectiveFormatSet(attrs goipp.Attributes, policy config.PolicyConfig) (map[string]struct{}, bool) {
	filtered := synthesizedFormatAttributes(attrs, policy, nil, false)
	formatAttr, present := iattr.Attr(filtered, "document-format-supported")
	if !present {
		return nil, false
	}
	formats := make(map[string]struct{}, len(formatAttr.Values))
	for _, value := range formatAttr.Values {
		if value.T != goipp.TagMimeType {
			continue
		}
		format, ok := value.V.(goipp.String)
		if !ok {
			continue
		}
		format = goipp.String(strings.TrimSpace(string(format)))
		if format == "" || strings.EqualFold(string(format), "application/octet-stream") {
			continue
		}
		formats[strings.ToLower(string(format))] = struct{}{}
	}
	return formats, true
}

func usableProfileMedia(upstream goipp.Attributes, policy config.PolicyConfig) bool {
	for _, name := range policyMediaNames(policy) {
		if policyMediaSupportedByUpstream(upstream, policy, name) {
			return true
		}
	}
	return false
}

func policyMediaSupportedByUpstream(upstream goipp.Attributes, policy config.PolicyConfig, name string) bool {
	if policy.UseMediaCol {
		return upstreamSupportsMedia(upstream, name)
	}
	attr, ok := iattr.Attr(upstream, "media-supported")
	return ok && hasKeywordString(attr, name)
}

func usableProfileIdentity(proxyURI string) bool {
	parsed, err := url.Parse(proxyURI)
	return err == nil && strings.EqualFold(parsed.Scheme, "ipp") && parsed.Hostname() != "" && proxyPrinterUUID(proxyURI) != ""
}

func hasProfileOperation(operations []goipp.Op, wanted goipp.Op) bool {
	return operationAvailable(operations, wanted)
}

func evaluateCapabilityProfiles(upstream goipp.Attributes, printer config.PrinterConfig, policy config.PolicyConfig, operations []goipp.Op, proxyURI string, disabled bool) CapabilityProfiles {
	profiles := CapabilityProfiles{
		Ordinary:      CapabilityProfile{Enabled: !disabled, Ready: !disabled},
		AirPrint:      CapabilityProfile{Enabled: normalizeAirPrintMode(printer.AirPrintMode) != config.AirPrintDisabled},
		IPPEverywhere: CapabilityProfile{Enabled: normalizeIPPEverywhereMode(printer.IPPEverywhereMode) != config.IPPEverywhereDisabled},
	}
	if disabled {
		profiles.Ordinary.Reason = "queue is unavailable"
		if profiles.AirPrint.Enabled {
			profiles.AirPrint.Reason = "queue is unavailable"
		} else {
			profiles.AirPrint.Reason = "disabled by configuration (airprint_mode=disabled)"
		}
		if profiles.IPPEverywhere.Enabled {
			profiles.IPPEverywhere.Reason = "queue is unavailable"
		} else {
			profiles.IPPEverywhere.Reason = "disabled by configuration (ipp_everywhere_mode=disabled)"
		}
		return profiles
	}

	mediaReady := usableProfileMedia(upstream, policy)
	formats, formatAttributePresent := effectiveFormatSet(upstream, policy)
	formatReady := formatAttributePresent && len(formats) > 0
	identityReady := usableProfileIdentity(proxyURI)
	baseline := hasProfileOperation(operations, goipp.OpGetPrinterAttributes) && hasProfileOperation(operations, goipp.OpPrintJob)
	claimReady, claimReason := ippEverywhereClaimEligibility(upstream)
	routes := buildRouteSnapshot(upstream, policy, printer)

	if !profiles.AirPrint.Enabled {
		profiles.AirPrint.Reason = "disabled by configuration (airprint_mode=disabled)"
	} else {
		switch {
		case !baseline:
			profiles.AirPrint.Reason = "Get-Printer-Attributes and Print-Job are not both dispatchable"
		case !formatReady:
			profiles.AirPrint.Reason = "upstream has no admitted document format"
		case !mediaReady:
			profiles.AirPrint.Reason = "configured media is not supported by upstream"
		case !identityReady:
			profiles.AirPrint.Reason = "proxy printer identity is unavailable"
		case routes.AirPrintPath == AirPrintPathUnavailable:
			profiles.AirPrint.Reason = routes.Reason
		default:
			profiles.AirPrint.Ready = true
			if routes.AirPrintPath == AirPrintPathNative && strings.EqualFold(strings.TrimSpace(policy.PrintColorMode), "monochrome") && urfHasColorSpaces(upstream) {
				profiles.AirPrint.Warnings = append(profiles.AirPrint.Warnings,
					"upstream URF advertises color-coded values while policy is monochrome; the client view retains only policy-compatible URF tokens")
			}
		}
	}

	if !profiles.IPPEverywhere.Enabled {
		profiles.IPPEverywhere.Reason = "disabled by configuration (ipp_everywhere_mode=disabled)"
	} else {
		switch {
		case !claimReady:
			profiles.IPPEverywhere.Reason = claimReason
		case !baseline:
			profiles.IPPEverywhere.Reason = "Get-Printer-Attributes and Print-Job are not both dispatchable"
		case !formatReady:
			profiles.IPPEverywhere.Reason = "upstream has no admitted document format"
		case !mediaReady:
			profiles.IPPEverywhere.Reason = "configured media is not supported by upstream"
		case !identityReady:
			profiles.IPPEverywhere.Reason = "proxy printer identity is unavailable"
		default:
			profiles.IPPEverywhere.Ready = true
			if missing := missingOperations(operations, ippEverywhereRequiredOperations); len(missing) > 0 {
				profiles.IPPEverywhere.Warnings = append(profiles.IPPEverywhere.Warnings,
					fmt.Sprintf("upstream/proxy operation surface is short of formal IPP Everywhere coverage: %s", operationNames(missing)))
			}
		}
		if claimReady && !profiles.IPPEverywhere.Ready && profiles.IPPEverywhere.Reason != "" {
			profiles.IPPEverywhere.Warnings = append(profiles.IPPEverywhere.Warnings,
				"upstream claims ipp-everywhere but the proxy cannot advertise the profile: "+profiles.IPPEverywhere.Reason)
		}
	}
	return profiles
}

func sameCapabilityProfiles(left, right CapabilityProfiles) bool {
	return sameCapabilityProfile(left.Ordinary, right.Ordinary) &&
		sameCapabilityProfile(left.AirPrint, right.AirPrint) &&
		sameCapabilityProfile(left.IPPEverywhere, right.IPPEverywhere)
}

func sameCapabilityProfile(left, right CapabilityProfile) bool {
	if left.Enabled != right.Enabled || left.Ready != right.Ready || left.Reason != right.Reason || len(left.Warnings) != len(right.Warnings) {
		return false
	}
	for index := range left.Warnings {
		if left.Warnings[index] != right.Warnings[index] {
			return false
		}
	}
	return true
}

func samePolicySnapshot(left, right config.PolicyConfig) bool {
	if !strings.EqualFold(left.Media, right.Media) || !strings.EqualFold(left.MediaDefault, right.MediaDefault) ||
		!strings.EqualFold(left.MediaType, right.MediaType) || !strings.EqualFold(left.PrintColorMode, right.PrintColorMode) ||
		!strings.EqualFold(policyMediaSource(left), policyMediaSource(right)) || !strings.EqualFold(policyPrintScaling(left), policyPrintScaling(right)) ||
		left.UseMediaCol != right.UseMediaCol || !strings.EqualFold(left.FidelityMode, right.FidelityMode) || len(left.MediaSupported) != len(right.MediaSupported) {
		return false
	}
	for index := range left.MediaSupported {
		if !strings.EqualFold(left.MediaSupported[index], right.MediaSupported[index]) {
			return false
		}
	}
	return true
}

func policyPrintScaling(policy config.PolicyConfig) string {
	if policy.PrintScaling == nil {
		return ""
	}
	return *policy.PrintScaling
}

func operationNames(operations []goipp.Op) string {
	values := make([]string, 0, len(operations))
	for _, operation := range operations {
		values = append(values, operation.String())
	}
	return strings.Join(values, ",")
}

func urfHasColorSpaces(attrs goipp.Attributes) bool {
	attribute, ok := iattr.Attr(attrs, "urf-supported")
	if !ok {
		return false
	}
	for _, value := range attribute.Values {
		text, ok := value.V.(goipp.String)
		if !ok {
			continue
		}
		tokens, ok := splitURFTokens(string(text))
		if !ok {
			continue
		}
		for _, token := range tokens {
			if urfColorSpaceToken.MatchString(token) {
				return true
			}
		}
	}
	return false
}
