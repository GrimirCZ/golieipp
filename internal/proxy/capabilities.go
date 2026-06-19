package proxy

import (
	"fmt"
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
	"media-supported",
	"media-default",
	"media-col-supported",
	"media-col-default",
	"media-col-database",
	"media-type-supported",
	"media-type-default",
	"media-source-supported",
	"media-source-default",
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

func FilterPrinterAttributes(upstream goipp.Attributes, queueName, proxyURI string, printer config.PrinterConfig) goipp.Attributes {
	out := iattr.DropAttrs(upstream, constrainedPrinterAttrs...)
	out = append(out,
		iattr.URI("printer-uri-supported", proxyURI),
		iattr.Name("printer-name", printer.DisplayName),
		iattr.Text("printer-info", fmt.Sprintf("%s via policy proxy", printer.DisplayName)),
		iattr.Text("printer-location", printer.Location),
	)
	out = append(out,
		iattr.Keyword("media-supported", printer.Policy.Media),
		iattr.Keyword("media-default", printer.Policy.Media),
		iattr.Keyword("media-col-supported", "media-size"),
		iattr.A4MediaColNamed("media-col-default", printer.Policy.MediaType),
		iattr.A4MediaColNamed("media-col-database", printer.Policy.MediaType),
		iattr.Keyword("media-type-supported", printer.Policy.MediaType),
		iattr.Keyword("media-type-default", printer.Policy.MediaType),
		iattr.Keyword("print-color-mode-supported", printer.Policy.PrintColorMode),
		iattr.Keyword("print-color-mode-default", printer.Policy.PrintColorMode),
		iattr.Boolean("color-supported", false),
		iattr.Keyword("output-mode-supported", "monochrome"),
		iattr.Keyword("output-mode-default", "monochrome"),
		operationsSupportedAttr(),
		iattr.Boolean("multiple-document-jobs-supported", false),
		iattr.Keyword("pwg-raster-document-type-supported", "sgray_8"),
		iattr.Keywords("urf-supported", "W8-16"),
	)
	if printer.Policy.MediaSource != nil && *printer.Policy.MediaSource != "" {
		out = append(out,
			iattr.Keyword("media-source-supported", *printer.Policy.MediaSource),
			iattr.Keyword("media-source-default", *printer.Policy.MediaSource),
		)
	}
	return out
}

func operationsSupportedAttr() goipp.Attribute {
	ops := []goipp.Op{
		goipp.OpPrintJob,
		goipp.OpValidateJob,
		goipp.OpCancelJob,
		goipp.OpGetJobAttributes,
		goipp.OpGetJobs,
		goipp.OpGetPrinterAttributes,
	}
	values := make([]goipp.Value, len(ops))
	for i, op := range ops {
		values[i] = goipp.Integer(op)
	}
	return goipp.MakeAttr("operations-supported", goipp.TagEnum, values[0], values[1:]...)
}

func ValidatePolicyAgainstUpstream(attrs goipp.Attributes, printer config.PrinterConfig) error {
	if !iattr.HasStringValue(attrs, "media-supported", printer.Policy.Media) {
		return fmt.Errorf("upstream does not advertise media-supported=%s", printer.Policy.Media)
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
	if printer.Policy.MediaSource != nil && *printer.Policy.MediaSource != "" {
		if !iattr.HasStringValue(attrs, "media-source-supported", *printer.Policy.MediaSource) {
			return fmt.Errorf("upstream does not advertise media-source-supported=%s", *printer.Policy.MediaSource)
		}
	}
	return nil
}

func hasString(attr goipp.Attribute, value string) bool {
	for _, val := range attr.Values {
		if s, ok := val.V.(goipp.String); ok && strings.EqualFold(string(s), value) {
			return true
		}
	}
	return false
}
