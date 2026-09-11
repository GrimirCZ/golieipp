package proxy

import (
	"sort"
	"strings"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func printerDNSSDTXT(resourcePath, name, location string, attrs goipp.Attributes) map[string]string {
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
	if urf := stringValues(attrs, "urf-supported"); len(urf) > 0 {
		txt["URF"] = strings.Join(urf, ",")
	}
	return txt
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
