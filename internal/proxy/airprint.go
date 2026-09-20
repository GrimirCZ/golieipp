package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
	"github.com/grimir/golieipp/internal/urf"
)

type translatedDocument struct {
	file     *os.File
	path     string
	result   urf.Result
	duration time.Duration
}

func (d *translatedDocument) Reader() io.Reader {
	if d == nil || d.file == nil {
		return nil
	}
	return d.file
}

func (d *translatedDocument) Close() error {
	if d == nil {
		return nil
	}
	var joined error
	if d.file != nil {
		joined = errors.Join(joined, d.file.Close())
	}
	if d.path != "" {
		joined = errors.Join(joined, os.Remove(d.path))
	}
	return joined
}

// translatePayload fully validates and stages an emulated document before an
// upstream request is constructed. The caller owns cleanup through Close.
func translatePayload(ctx context.Context, payload io.Reader, route DocumentRoute, attrs goipp.Attributes, upstream goipp.Attributes, maxBytes int64) (*translatedDocument, error) {
	if !route.Transform {
		return nil, nil
	}
	if payload == nil {
		return nil, fmt.Errorf("emulated document payload is missing")
	}
	file, err := os.CreateTemp("", "golieipp-airprint-*")
	if err != nil {
		return nil, fmt.Errorf("create AirPrint translation staging file: %w", err)
	}
	doc := &translatedDocument{file: file, path: file.Name()}
	remove := true
	defer func() {
		if remove {
			_ = doc.Close()
		}
	}()

	settings := routePageSettings(route, attrs, upstream)
	limits := urf.DefaultLimits()
	if maxBytes > 0 {
		limits.MaxInputBytes = maxBytes
		limits.MaxOutputBytes = maxBytes
	}
	start := time.Now()
	result, translateErr := urf.Translate(ctx, payload, file, urf.Options{
		Mapping: route.Mapping,
		Page:    settings,
		Limits:  limits,
	})
	if translateErr != nil {
		return nil, translateErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind AirPrint translation staging file: %w", err)
	}
	doc.result = result
	doc.duration = time.Since(start)
	remove = false
	return doc, nil
}

func translationErrorProtocol(req *goipp.Message, err error) *ProtocolError {
	if err == nil {
		return nil
	}
	status := goipp.StatusErrorDocumentFormatError
	message := "AirPrint document translation failed"
	switch urf.ErrorKindOf(err) {
	case urf.KindMalformedInput:
		status = goipp.StatusErrorDocumentFormatError
		message = "AirPrint document is malformed"
	case urf.KindUnsupportedInput, urf.KindSettingsMismatch, urf.KindInvalidOptions:
		status = goipp.StatusErrorDocumentFormatNotSupported
		message = "AirPrint document format is not supported by this queue"
	case urf.KindResourceLimit:
		status = goipp.StatusErrorRequestEntity
		message = "AirPrint document exceeds the configured translation limit"
	case urf.KindCanceled:
		status = goipp.StatusErrorServiceUnavailable
		message = "AirPrint document translation was canceled"
	case urf.KindSourceIO, urf.KindDestinationIO:
		status = goipp.StatusErrorServiceUnavailable
		message = "AirPrint document could not be staged for translation"
	default:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = goipp.StatusErrorServiceUnavailable
			message = "AirPrint document translation was canceled"
		}
	}
	if req == nil {
		return &ProtocolError{Status: status, Message: message}
	}
	return &ProtocolError{Status: status, Message: message, Unsupported: attrsNamed(req.Operation, "document-format")}
}

func routeToStore(route DocumentRoute) store.DocumentRoute {
	kind := store.RoutePassThrough
	if route.Transform {
		kind = store.RouteURFToPWG
	}
	return store.DocumentRoute{
		ClientDocumentFormat:   strings.ToLower(strings.TrimSpace(route.ClientFormat)),
		UpstreamDocumentFormat: strings.ToLower(strings.TrimSpace(route.UpstreamFormat)),
		Kind:                   kind,
		Mapping:                route.Mapping.String(),
		MediaName:              route.Page.MediaName,
		MediaWidth:             route.Page.MediaWidth,
		MediaHeight:            route.Page.MediaHeight,
		MediaType:              route.Page.MediaType,
		MediaSource:            route.Page.MediaSource,
		ResolutionX:            route.Page.ResolutionX,
		ResolutionY:            route.Page.ResolutionY,
		PrintQuality:           route.Page.PrintQuality,
		Sides:                  route.Page.Sides,
		SheetBack:              route.Page.SheetBack,
	}
}

func routeKind(route DocumentRoute) string {
	if route.Transform {
		return store.RouteURFToPWG
	}
	return store.RoutePassThrough
}

func routeFromStore(job store.Job) (DocumentRoute, bool) {
	stored := job.Route
	if stored.ClientDocumentFormat == "" {
		stored.ClientDocumentFormat = job.DocumentFormat
	}
	if stored.UpstreamDocumentFormat == "" {
		stored.UpstreamDocumentFormat = job.UpstreamDocumentFormat
	}
	if stored.UpstreamDocumentFormat == "" {
		stored.UpstreamDocumentFormat = stored.ClientDocumentFormat
	}
	if stored.Kind == "" {
		stored.Kind = job.RouteKind
	}
	if stored.Kind == "" {
		stored.Kind = store.RoutePassThrough
	}
	if stored.Mapping == "" {
		stored.Mapping = job.RouteMapping
	}
	if stored.MediaName == "" {
		stored.MediaName = job.RouteMediaName
	}
	if stored.MediaWidth == 0 {
		stored.MediaWidth = job.RouteMediaWidth
	}
	if stored.MediaHeight == 0 {
		stored.MediaHeight = job.RouteMediaHeight
	}
	if stored.MediaType == "" {
		stored.MediaType = job.RouteMediaType
	}
	if stored.MediaSource == 0 {
		stored.MediaSource = job.RouteMediaSource
	}
	if stored.ResolutionX == 0 {
		stored.ResolutionX = job.RouteResolutionX
	}
	if stored.ResolutionY == 0 {
		stored.ResolutionY = job.RouteResolutionY
	}
	if stored.PrintQuality == 0 {
		stored.PrintQuality = job.RoutePrintQuality
	}
	if stored.Sides == "" {
		stored.Sides = job.RouteSides
	}
	if stored.SheetBack == "" {
		stored.SheetBack = job.RouteSheetBack
	}
	result := DocumentRoute{
		ClientFormat: stored.ClientDocumentFormat, UpstreamFormat: stored.UpstreamDocumentFormat,
		Transform: stored.Kind == store.RouteURFToPWG,
		Mapping:   mappingFromString(stored.Mapping),
		Page: urf.PageSettings{
			MediaName: stored.MediaName, MediaWidth: stored.MediaWidth, MediaHeight: stored.MediaHeight,
			MediaType: stored.MediaType, MediaSource: stored.MediaSource, ResolutionX: stored.ResolutionX,
			ResolutionY: stored.ResolutionY, PrintQuality: stored.PrintQuality, Sides: stored.Sides,
			SheetBack: stored.SheetBack,
		},
	}
	if result.ClientFormat == "" || result.UpstreamFormat == "" {
		return DocumentRoute{}, false
	}
	return result, true
}

func mappingFromString(value string) urf.Mapping {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case strings.ToLower(urf.MappingW8ToSGray8.String()):
		return urf.MappingW8ToSGray8
	case strings.ToLower(urf.MappingSRGB24ToSRGB8.String()):
		return urf.MappingSRGB24ToSRGB8
	case strings.ToLower(urf.MappingDEVRGB24ToRGB8.String()):
		return urf.MappingDEVRGB24ToRGB8
	default:
		return 0
	}
}

func (s *Service) routeForRequest(queue, format string) (DocumentRoute, bool) {
	s.mu.RLock()
	model, hasModel := s.capabilityModels[queue]
	upstream := s.capabilities[queue]
	printer, hasPrinter := s.cfg.Printers[queue]
	s.mu.RUnlock()
	if hasModel {
		snapshot := routeSnapshotForModel(model)
		if strings.TrimSpace(format) == "" {
			return snapshot.defaultRoute(model.Upstream)
		}
		return snapshot.routeFor(format)
	}
	if !hasPrinter {
		return DocumentRoute{}, false
	}
	if upstream == nil {
		upstream = s.upstreamCapabilities(queue)
	}
	snapshot := buildRouteSnapshot(upstream, printer.Policy, printer)
	if strings.TrimSpace(format) == "" {
		if route, found := snapshot.defaultRoute(upstream); found {
			return route, true
		}
		return legacyPassThroughRoute(upstream, format)
	}
	if route, found := snapshot.routeFor(format); found {
		return route, true
	}
	return legacyPassThroughRoute(upstream, format)
}

func (s *Service) recordTranslationError(queue string, err error) {
	s.mu.Lock()
	health := s.queueHealth[queue]
	if err != nil {
		health.LastTranslationError = err.Error()
	}
	s.queueHealth[queue] = health
	s.mu.Unlock()
}

func (s *Service) clientRoute(queue, format string, req *goipp.Message) (DocumentRoute, *ProtocolError) {
	route, ok := s.routeForRequest(queue, format)
	if !ok {
		return DocumentRoute{}, &ProtocolError{
			Status:      goipp.StatusErrorDocumentFormatNotSupported,
			Message:     "document-format is not admitted for this queue",
			Unsupported: attrsNamed(req.Operation, "document-format"),
		}
	}
	return route, nil
}

func rewriteDocumentFormat(attrs goipp.Attributes, format string) goipp.Attributes {
	if strings.TrimSpace(format) == "" {
		return attrs
	}
	return iattr.SetAttr(attrs, goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
}
