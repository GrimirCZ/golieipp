package proxy

import (
	"log/slog"

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
	ClientMediaType      string
	ClientMediaSource    string
	ClientPrintColorMode string
	ForcedMedia          string
	ForcedMediaType      string
	ForcedMediaSource    string
	ForcedPrintColorMode string
}

func NormalizeJobAttrs(attrs goipp.Attributes, policy config.PolicyConfig, vendorDrop []string) (goipp.Attributes, NormalizationLog) {
	log := NormalizationLog{
		ForcedMedia:          policy.Media,
		ForcedMediaType:      policy.MediaType,
		ForcedPrintColorMode: policy.PrintColorMode,
	}
	if policy.MediaSource != nil {
		log.ForcedMediaSource = *policy.MediaSource
	}
	log.ClientMedia, _ = iattr.FirstString(attrs, "media")
	log.ClientMediaType, _ = iattr.FirstString(attrs, "media-type")
	log.ClientMediaSource, _ = iattr.FirstString(attrs, "media-source")
	log.ClientPrintColorMode, _ = iattr.FirstString(attrs, "print-color-mode")

	drops := append([]string{}, policyDropAttrs...)
	drops = append(drops, vendorDrop...)
	out := iattr.DropAttrs(attrs, drops...)

	if policy.UseMediaCol {
		out = append(out, iattr.A4MediaCol(policy.MediaType))
	} else {
		out = append(out, iattr.Keyword("media", policy.Media))
		if policy.MediaType != "" {
			out = append(out, iattr.Keyword("media-type", policy.MediaType))
		}
	}
	if policy.MediaSource != nil && *policy.MediaSource != "" {
		out = append(out, iattr.Keyword("media-source", *policy.MediaSource))
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

func (n NormalizationLog) Attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("client_media", n.ClientMedia),
		slog.String("client_media_type", n.ClientMediaType),
		slog.String("client_media_source", n.ClientMediaSource),
		slog.String("client_color", n.ClientPrintColorMode),
		slog.String("forced_media", n.ForcedMedia),
		slog.String("forced_media_type", n.ForcedMediaType),
		slog.String("forced_media_source", n.ForcedMediaSource),
		slog.String("forced_color", n.ForcedPrintColorMode),
	}
}
