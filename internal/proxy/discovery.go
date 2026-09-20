package proxy

import (
	"sort"
	"strings"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func printerDNSSDTXT(resourcePath, name, location string, attrs goipp.Attributes) map[string]string {
	return printerDNSSDTXTForProfiles(resourcePath, name, location, attrs, CapabilityProfiles{
		Ordinary:      CapabilityProfile{Ready: true},
		AirPrint:      CapabilityProfile{Ready: true},
		IPPEverywhere: CapabilityProfile{Ready: true},
	})
}

func printerDNSSDTXTForProfiles(resourcePath, name, location string, attrs goipp.Attributes, profiles CapabilityProfiles) map[string]string {
	model := name
	if upstreamModel, ok := iattr.FirstString(attrs, "printer-make-and-model"); ok && strings.TrimSpace(upstreamModel) != "" {
		model = upstreamModel
	}
	txt := map[string]string{
		"txtvers": "1",
		"qtotal":  "1",
		"rp":      strings.TrimLeft(resourcePath, "/"),
		"ty":      model,
	}
	if location != "" {
		txt["note"] = location
	}
	formats := stringValues(attrs, "document-format-supported")
	filtered := formats[:0]
	for _, format := range formats {
		if !strings.EqualFold(format, "application/octet-stream") {
			filtered = append(filtered, format)
		}
	}
	sort.Strings(filtered)
	if len(filtered) > 0 {
		txt["pdl"] = strings.Join(filtered, ",")
	}
	if containsFold(stringValues(attrs, "print-color-mode-supported"), "color") {
		txt["Color"] = "T"
	} else {
		txt["Color"] = "F"
	}
	if containsFold(stringValues(attrs, "sides-supported"), "two-sided-long-edge") || containsFold(stringValues(attrs, "sides-supported"), "two-sided-short-edge") {
		txt["Duplex"] = "T"
	} else {
		txt["Duplex"] = "F"
	}
	if uuid, ok := iattr.FirstString(attrs, "printer-uuid"); ok {
		txt["UUID"] = strings.TrimPrefix(strings.ToLower(uuid), "urn:uuid:")
	}
	if model, ok := iattr.FirstString(attrs, "printer-make-and-model"); ok && model != "" {
		txt["product"] = "(" + model + ")"
	}
	if profiles.AirPrint.Ready {
		if urf := stringValues(attrs, "urf-supported"); len(urf) > 0 && validAdvertisedURFFamily(attrs) {
			txt["URF"] = strings.Join(urf, ",")
		}
	}
	return txt
}

// validAdvertisedURFFamily accepts the strict upstream family as well as the
// proxy's synthesized grayscale form. A synthesized W8 route intentionally
// has no separate color-space token: W8 itself is the selected grayscale
// flavor, and adding a color token would advertise a mapping the policy did
// not admit.
func validAdvertisedURFFamily(attrs goipp.Attributes) bool {
	if validURFFamily(attrs) {
		return true
	}
	formats, ok := iattr.Attr(attrs, "document-format-supported")
	if !ok || !hasMimeTypeValue(formats, "image/urf") {
		return false
	}
	attr, ok := iattr.Attr(attrs, "urf-supported")
	if !ok || len(attr.Values) == 0 {
		return false
	}
	width, resolution := false, false
	tokens := 0
	for _, value := range attr.Values {
		if value.T != goipp.TagKeyword {
			return false
		}
		text, ok := value.V.(goipp.String)
		if !ok {
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
			case urfColorSpaceToken.MatchString(token), urfVersionToken.MatchString(token), urfPositiveNumericListToken.MatchString(token), urfOptionalZeroToken.MatchString(token):
			default:
				return false
			}
		}
	}
	return tokens >= 2 && width && resolution
}

func stringValues(attrs goipp.Attributes, name string) []string {
	attr, ok := iattr.Attr(attrs, name)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(attr.Values))
	for _, value := range attr.Values {
		if text, ok := value.V.(goipp.String); ok {
			out = append(out, string(text))
		}
	}
	return out
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}
