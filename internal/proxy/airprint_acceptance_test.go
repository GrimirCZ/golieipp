package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

// These tests deliberately use the public HTTP path and a small fake upstream.
// They are kept separate from the focused unit tests so a passing package suite
// also proves the client-visible AirPrint route, complete staging, and upstream
// dispatch contract together.

func TestAirPrintAcceptanceEmulatesURFAndStagesBeforeDispatch(t *testing.T) {
	upstreamAttrs := acceptanceRasterCapabilities("monochrome", false, true)
	upstream := newAcceptanceUpstream(t, upstreamAttrs)
	defer upstream.Close()

	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}
	printerResponse := acceptanceIPP(t, svc, goipp.OpGetPrinterAttributes, 11, nil, "", 0, false)
	if status := goipp.Status(printerResponse.Code); status != goipp.StatusOk {
		t.Fatalf("Get-Printer-Attributes status = %s", status)
	}
	if !iattr.HasStringValue(printerResponse.Printer, "document-format-supported", "image/urf") {
		t.Fatalf("emulated client view omitted image/urf: %+v", printerResponse.Printer)
	}
	urf, ok := iattr.Attr(printerResponse.Printer, "urf-supported")
	if !ok || !acceptanceHasURFToken(urf, "W8") || !acceptanceHasURFToken(urf, "V1.4") || !acceptanceHasURFToken(urf, "RS72") {
		t.Fatalf("emulated client view has incomplete URF family: %+v", urf)
	}

	document := acceptanceURF(595, 841, 72, 0, 8)
	response := acceptanceIPP(t, svc, goipp.OpPrintJob, 12, document, "image/urf", 0, false)
	status := goipp.Status(response.Code)
	if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("emulated Print-Job status = %s, message=%q", status, statusMessage(response))
	}

	record := upstream.lastOperation(goipp.OpPrintJob)
	if record == nil {
		t.Fatal("upstream did not receive Print-Job")
	}
	if got, _ := iattr.FirstString(record.operation, "document-format"); !strings.EqualFold(got, "image/pwg-raster") {
		t.Fatalf("upstream document-format = %q, want image/pwg-raster", got)
	}
	if !bytes.HasPrefix(record.payload, []byte("RaS2")) {
		t.Fatalf("upstream payload lacks PWG marker: %q", record.payload[:minAcceptance(8, len(record.payload))])
	}
	if len(record.payload) < 1800 || !bytes.Equal(record.payload[1800:], document[44:]) {
		t.Fatalf("translated raster payload was not packet-preserving after the PWG header")
	}
	if got := upstream.countOperation(goipp.OpPrintJob); got != 1 {
		t.Fatalf("upstream Print-Job count = %d, want one", got)
	}
}

func TestAirPrintAcceptanceMalformedURFDoesNotDispatch(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	response := acceptanceIPP(t, svc, goipp.OpPrintJob, 21, []byte("UNIR\x00bad"), "image/urf", 0, false)
	status := goipp.Status(response.Code)
	if status == goipp.StatusOk || status == goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("malformed URF unexpectedly succeeded: %s", status)
	}
	if got := upstream.countOperation(goipp.OpPrintJob); got != 0 {
		t.Fatalf("malformed URF reached upstream %d times", got)
	}
}

func TestAirPrintAcceptancePrefersNativeURF(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", true, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	document := acceptanceURF(595, 841, 72, 0, 8)
	response := acceptanceIPP(t, svc, goipp.OpPrintJob, 31, document, "image/urf", 0, false)
	status := goipp.Status(response.Code)
	if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("native Print-Job status = %s, message=%q", status, statusMessage(response))
	}
	record := upstream.lastOperation(goipp.OpPrintJob)
	if record == nil {
		t.Fatal("upstream did not receive native Print-Job")
	}
	if got, _ := iattr.FirstString(record.operation, "document-format"); !strings.EqualFold(got, "image/urf") {
		t.Fatalf("native URF was rewritten to %q", got)
	}
	if !bytes.Equal(record.payload, document) {
		t.Fatal("native URF payload was changed")
	}
}

func TestAirPrintAcceptancePinnedRouteSurvivesRefreshAndRestart(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	databasePath := t.TempDir() + "/jobs.db"
	jobStore, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	first := newAcceptanceServiceWithStore(t, upstream, config.AirPrintEmulateIfMissing, "monochrome", jobStore)
	if err := first.RefreshAll(context.Background()); err != nil {
		t.Fatalf("initial RefreshAll() = %v", err)
	}
	created := acceptanceIPP(t, first, goipp.OpCreateJob, 61, nil, "image/urf", 0, false,
		iattr.Keyword("media", "iso_a5_148x210mm"),
		iattr.Keyword("sides", "two-sided-long-edge"),
		goipp.MakeAttribute("printer-resolution", goipp.TagResolution, goipp.Resolution{Xres: 150, Yres: 150, Units: goipp.UnitsDpi}),
	)
	proxyJobID, ok := iattr.FirstInt(created.Job, "job-id")
	if !ok || proxyJobID < 1 {
		t.Fatalf("Create-Job response has no proxy job-id: %+v", created.Job)
	}
	pinned, pinErr := jobStore.SelectedRoute(context.Background(), "office", proxyJobID)
	if pinErr != nil {
		t.Fatal(pinErr)
	}
	if pinned.MediaName != "iso_a5_148x210mm" || pinned.ResolutionX != 150 || pinned.ResolutionY != 150 || pinned.Sides != "two-sided-long-edge" {
		t.Fatalf("Create-Job did not pin requested page settings: %+v", pinned)
	}
	if got := upstream.lastOperation(goipp.OpCreateJob); got == nil || !strings.EqualFold(acceptanceOperationFormat(got.operation), "image/pwg-raster") {
		t.Fatalf("Create-Job was not routed to PWG: %+v", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first service Close() = %v", err)
	}
	if err := jobStore.Close(); err != nil {
		t.Fatalf("first store Close() = %v", err)
	}

	// A refresh exposes native URF after the restart. The job's persisted route
	// must continue using the transform selected by Create-Job.
	upstream.setAttrs(acceptanceRasterCapabilities("monochrome", true, true))
	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	second := newAcceptanceServiceWithStore(t, upstream, config.AirPrintEmulateIfMissing, "monochrome", reopened)
	defer reopened.Close()
	defer second.Close()
	if err := second.RefreshAll(context.Background()); err != nil {
		t.Fatalf("post-restart RefreshAll() = %v", err)
	}
	// 148x210mm at 150dpi, using the translator's integer hundredths-mm
	// conversion, is 874x1240 pixels. The route must retain this requested
	// media, duplex mode, and resolution through refresh and SQLite reopen.
	document := acceptanceURF(874, 1240, 150, 0, 8)
	document[14] = 3 // long-edge duplex in the URF page header
	sent := acceptanceIPP(t, second, goipp.OpSendDocument, 62, document, "image/urf", proxyJobID, true)
	status := goipp.Status(sent.Code)
	if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("pinned Send-Document status = %s, message=%q", status, statusMessage(sent))
	}
	record := upstream.lastOperation(goipp.OpSendDocument)
	if record == nil {
		t.Fatal("upstream did not receive Send-Document")
	}
	if got := acceptanceOperationFormat(record.operation); !strings.EqualFold(got, "image/pwg-raster") {
		t.Fatalf("persisted route changed after refresh/restart: upstream format %q", got)
	}
	if !bytes.HasPrefix(record.payload, []byte("RaS2")) {
		t.Fatal("pinned Send-Document payload was not translated")
	}
}

func TestAirPrintAcceptanceOmittedFormatUsesAdvertisedDefaultAndPins(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	created := acceptanceIPP(t, svc, goipp.OpCreateJob, 71, nil, "", 0, false)
	proxyJobID, ok := iattr.FirstInt(created.Job, "job-id")
	if !ok || proxyJobID < 1 {
		t.Fatalf("omitted-format Create-Job response has no proxy job-id: %+v", created.Job)
	}
	if got := acceptanceOperationFormat(upstream.lastOperation(goipp.OpCreateJob).operation); !strings.EqualFold(got, "image/pwg-raster") {
		t.Fatalf("omitted Create-Job used %q, want advertised image/pwg-raster default", got)
	}

	payload := []byte("already-pwg")
	sent := acceptanceIPP(t, svc, goipp.OpSendDocument, 72, payload, "", proxyJobID, true)
	if status := goipp.Status(sent.Code); status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("omitted-format Send-Document status = %s, message=%q", status, statusMessage(sent))
	}
	record := upstream.lastOperation(goipp.OpSendDocument)
	if record == nil || !strings.EqualFold(acceptanceOperationFormat(record.operation), "image/pwg-raster") {
		t.Fatalf("omitted Send-Document did not retain the advertised default: %+v", record)
	}
	if !bytes.Equal(record.payload, payload) {
		t.Fatalf("omitted-format pass-through payload changed")
	}

	// An explicit URF submission after a default-format Create-Job conflicts
	// with the durable route selected at creation and must not reach upstream.
	second := acceptanceIPP(t, svc, goipp.OpCreateJob, 73, nil, "", 0, false)
	secondID, ok := iattr.FirstInt(second.Job, "job-id")
	if !ok || secondID < 1 {
		t.Fatalf("second omitted-format Create-Job response has no proxy job-id: %+v", second.Job)
	}
	before := upstream.countOperation(goipp.OpSendDocument)
	conflict := acceptanceIPP(t, svc, goipp.OpSendDocument, 74, []byte("UNIR-invalid"), "image/urf", secondID, true)
	if status := goipp.Status(conflict.Code); status != goipp.StatusErrorConflicting {
		t.Fatalf("explicit URF after default Create-Job status = %s, want conflicting-attributes", status)
	}
	if got := upstream.countOperation(goipp.OpSendDocument); got != before {
		t.Fatalf("conflicting explicit URF reached upstream: count=%d before=%d", got, before)
	}
}

func TestAirPrintAcceptanceColorMappingAndMonochromeFiltering(t *testing.T) {
	t.Run("sRGB-preferred-color-route", func(t *testing.T) {
		upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("color", false, true))
		defer upstream.Close()
		svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "color")
		defer svc.Close()
		if err := svc.RefreshAll(context.Background()); err != nil {
			t.Fatalf("RefreshAll() = %v", err)
		}

		attrs := acceptanceIPP(t, svc, goipp.OpGetPrinterAttributes, 41, nil, "", 0, false)
		urf, ok := iattr.Attr(attrs.Printer, "urf-supported")
		if !ok || !acceptanceHasURFToken(urf, "SRGB24") || acceptanceHasURFToken(urf, "W8") {
			t.Fatalf("color client view has wrong URF mapping: %+v", urf)
		}
		if !iattr.HasStringValue(attrs.Printer, "pwg-raster-document-type-supported", "srgb_8") {
			t.Fatalf("color client view omitted srgb_8: %+v", attrs.Printer)
		}

		document := acceptanceURF(595, 841, 72, 1, 24)
		response := acceptanceIPP(t, svc, goipp.OpPrintJob, 42, document, "image/urf", 0, false)
		status := goipp.Status(response.Code)
		if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
			t.Fatalf("color Print-Job status = %s, message=%q", status, statusMessage(response))
		}
		record := upstream.lastOperation(goipp.OpPrintJob)
		if record == nil {
			t.Fatal("upstream did not receive color Print-Job")
		}
		if got, _ := iattr.FirstString(record.operation, "document-format"); !strings.EqualFold(got, "image/pwg-raster") {
			t.Fatalf("color upstream document-format = %q", got)
		}
		if !bytes.HasPrefix(record.payload, []byte("RaS2")) || len(record.payload) < 1800 || !bytes.Equal(record.payload[1800:], document[44:]) {
			t.Fatal("color translation did not preserve packet payload after the PWG header")
		}
	})

	t.Run("monochrome-policy-removes-color", func(t *testing.T) {
		upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
		defer upstream.Close()
		svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
		defer svc.Close()
		if err := svc.RefreshAll(context.Background()); err != nil {
			t.Fatalf("RefreshAll() = %v", err)
		}

		attrs := acceptanceIPP(t, svc, goipp.OpGetPrinterAttributes, 51, nil, "", 0, false)
		urf, ok := iattr.Attr(attrs.Printer, "urf-supported")
		if !ok || !acceptanceHasURFToken(urf, "W8") || acceptanceHasURFToken(urf, "SRGB24") || acceptanceHasURFToken(urf, "DEVRGB24") {
			t.Fatalf("monochrome client view leaked color URF mapping: %+v", urf)
		}
		if iattr.HasStringValue(attrs.Printer, "pwg-raster-document-type-supported", "srgb_8") || iattr.HasStringValue(attrs.Printer, "pwg-raster-document-type-supported", "rgb_8") {
			t.Fatalf("monochrome client view leaked color PWG type: %+v", attrs.Printer)
		}
	})
}

type acceptanceUpstream struct {
	attrs goipp.Attributes

	mu       sync.Mutex
	requests []acceptanceRequest
	nextID   int
}

func (u *acceptanceUpstream) Close() {}

type acceptanceRequest struct {
	op        goipp.Op
	operation goipp.Attributes
	payload   []byte
}

func newAcceptanceUpstream(t *testing.T, attrs goipp.Attributes) *acceptanceUpstream {
	t.Helper()
	return &acceptanceUpstream{attrs: attrs, nextID: 70}
}

func (u *acceptanceUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	request := &goipp.Message{}
	if err := request.Decode(r.Body); err != nil {
		return nil, err
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	op := goipp.Op(request.Code)
	u.mu.Lock()
	attrs := u.attrs.DeepCopy()
	u.requests = append(u.requests, acceptanceRequest{op: op, operation: request.Operation.DeepCopy(), payload: append([]byte(nil), payload...)})
	id := u.nextID
	u.nextID++
	u.mu.Unlock()

	response := goipp.NewResponse(request.Version, goipp.StatusOk, request.RequestID)
	response.Operation = acceptanceResponseOperationAttrs("")
	if op == goipp.OpGetPrinterAttributes {
		response.Printer = attrs
	} else if op == goipp.OpPrintJob || op == goipp.OpCreateJob || op == goipp.OpSendDocument {
		response.Job = goipp.Attributes{iattr.Integer("job-id", id)}
	}
	envelope, err := response.EncodeBytes()
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{goipp.ContentType}},
		Body:       io.NopCloser(bytes.NewReader(envelope)),
		Request:    r,
	}, nil
}

func (u *acceptanceUpstream) countOperation(op goipp.Op) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	count := 0
	for _, request := range u.requests {
		if request.op == op {
			count++
		}
	}
	return count
}

func (u *acceptanceUpstream) lastOperation(op goipp.Op) *acceptanceRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	for index := len(u.requests) - 1; index >= 0; index-- {
		if u.requests[index].op == op {
			request := u.requests[index]
			return &acceptanceRequest{op: request.op, operation: request.operation.DeepCopy(), payload: append([]byte(nil), request.payload...)}
		}
	}
	return nil
}

func (u *acceptanceUpstream) setAttrs(attrs goipp.Attributes) {
	u.mu.Lock()
	u.attrs = attrs.DeepCopy()
	u.mu.Unlock()
}

func acceptanceOperationFormat(attrs goipp.Attributes) string {
	format, _ := iattr.FirstString(attrs, "document-format")
	return format
}

func newAcceptanceService(t *testing.T, upstream *acceptanceUpstream, mode, colorMode string) *Service {
	t.Helper()
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy.example:631/printers"},
		DNSSD:  config.DNSSDConfig{Mode: config.DNSModeOff},
		Printers: map[string]config.PrinterConfig{
			"office": {
				UpstreamURI:  "ipp://printer.example.test/ipp/print",
				DisplayName:  "Office",
				AirPrintMode: mode,
				Policy: config.PolicyConfig{
					Media:          "iso_a4_210x297mm",
					MediaType:      "stationery",
					PrintColorMode: colorMode,
				},
			},
		},
	}
	svc := newAcceptanceServiceWithStoreAndConfig(t, upstream, cfg, jobStore)
	t.Cleanup(func() { _ = jobStore.Close() })
	return svc
}

func newAcceptanceServiceWithStore(t *testing.T, upstream *acceptanceUpstream, mode, colorMode string, jobStore *store.Store) *Service {
	t.Helper()
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy.example:631/printers"},
		DNSSD:  config.DNSSDConfig{Mode: config.DNSModeOff},
		Printers: map[string]config.PrinterConfig{
			"office": {
				UpstreamURI:  "ipp://printer.example.test/ipp/print",
				DisplayName:  "Office",
				AirPrintMode: mode,
				Policy: config.PolicyConfig{
					MediaSupported: []string{"iso_a4_210x297mm", "iso_a5_148x210mm"},
					MediaDefault:   "iso_a4_210x297mm",
					MediaType:      "stationery",
					PrintColorMode: colorMode,
				},
				Passthrough: config.PassthroughConfig{PreserveJobAttrs: []string{"sides", "printer-resolution"}},
			},
		},
	}
	return newAcceptanceServiceWithStoreAndConfig(t, upstream, cfg, jobStore)
}

func newAcceptanceServiceWithStoreAndConfig(t *testing.T, upstream *acceptanceUpstream, cfg *config.Config, jobStore *store.Store) *Service {
	t.Helper()
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.upstream.HTTP = &http.Client{Transport: upstream}
	return svc
}

func acceptanceRasterCapabilities(colorMode string, nativeURF, bothColorTypes bool) goipp.Attributes {
	operations := []goipp.Value{
		goipp.Integer(goipp.OpGetPrinterAttributes), goipp.Integer(goipp.OpPrintJob),
		goipp.Integer(goipp.OpValidateJob), goipp.Integer(goipp.OpCreateJob),
		goipp.Integer(goipp.OpSendDocument), goipp.Integer(goipp.OpGetJobAttributes),
		goipp.Integer(goipp.OpCancelJob), goipp.Integer(goipp.OpGetJobs),
	}
	formats := []goipp.Value{goipp.String("application/pdf"), goipp.String("image/pwg-raster")}
	if nativeURF {
		formats = append(formats, goipp.String("image/urf"))
	}
	attrs := goipp.Attributes{
		goipp.MakeAttr("operations-supported", goipp.TagEnum, operations[0], operations[1:]...),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType, formats[0], formats[1:]...),
		goipp.MakeAttribute("document-format-default", goipp.TagMimeType, goipp.String("image/pwg-raster")),
		iattr.Keywords("media-supported", "iso_a4_210x297mm", "iso_a5_148x210mm"),
		iattr.Keyword("media-default", "iso_a4_210x297mm"),
		iattr.Keywords("media-ready", "iso_a4_210x297mm", "iso_a5_148x210mm"),
		goipp.MakeAttr("pwg-raster-document-resolution-supported", goipp.TagResolution,
			goipp.Resolution{Xres: 72, Yres: 72, Units: goipp.UnitsDpi},
			goipp.Resolution{Xres: 150, Yres: 150, Units: goipp.UnitsDpi}),
		iattr.Keyword("pwg-raster-document-sheet-back", "normal"),
		iattr.Keywords("sides-supported", "one-sided", "two-sided-long-edge", "two-sided-short-edge"),
		iattr.Keyword("print-color-mode-default", "color"),
		iattr.Name("printer-name", "Office"),
		iattr.Name("printer-make-and-model", "Acceptance Printer"),
	}
	attrs = append(attrs, iattr.Keywords("print-color-mode-supported", "monochrome", "color"))
	if colorMode == "monochrome" {
		if bothColorTypes {
			attrs = append(attrs, iattr.Keywords("pwg-raster-document-type-supported", "sgray_8", "srgb_8", "rgb_8"))
		} else {
			attrs = append(attrs, iattr.Keyword("pwg-raster-document-type-supported", "sgray_8"))
		}
	} else {
		if bothColorTypes {
			attrs = append(attrs, iattr.Keywords("pwg-raster-document-type-supported", "srgb_8", "rgb_8"))
		} else {
			attrs = append(attrs, iattr.Keyword("pwg-raster-document-type-supported", "srgb_8"))
		}
	}
	if nativeURF {
		attrs = append(attrs, iattr.Keywords("urf-supported", "V1.4", "W8", "DEVW8", "RS72"))
	}
	return attrs
}

func acceptanceResponseOperationAttrs(message string) goipp.Attributes {
	return goipp.Attributes{
		goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")),
		iattr.Text("status-message", message),
	}
}

func acceptanceIPP(t *testing.T, svc *Service, op goipp.Op, requestID uint32, document []byte, format string, jobID int, lastDocument bool, jobAttrs ...goipp.Attribute) *goipp.Message {
	t.Helper()
	request := goipp.NewRequest(goipp.DefaultVersion, op, requestID)
	request.Operation = append(request.Operation, acceptanceBasicOperationAttrs("ipp://proxy.example:631/printers/office")...)
	// IPP requires printer-uri/job-id to remain the third/fourth operation
	// attributes for Send-Document; document-format follows them.
	if op == goipp.OpSendDocument && jobID > 0 {
		request.Operation = append(request.Operation, iattr.Integer("job-id", jobID))
	}
	if format != "" {
		request.Operation = append(request.Operation, goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
	}
	if op != goipp.OpSendDocument && jobID > 0 {
		request.Operation = append(request.Operation, iattr.Integer("job-id", jobID))
	}
	if op == goipp.OpSendDocument {
		request.Operation = append(request.Operation, goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(lastDocument)))
	}
	request.Job = append(request.Job, jobAttrs...)
	envelope, err := request.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	body := io.MultiReader(bytes.NewReader(envelope), bytes.NewReader(document))
	httpRequest := httptest.NewRequest(http.MethodPost, "/printers/office", body)
	httpRequest.Header.Set("content-type", goipp.ContentType)
	httpRequest.ContentLength = int64(len(envelope) + len(document))
	recorder := httptest.NewRecorder()
	svc.Routes().ServeHTTP(recorder, httpRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, body=%q", recorder.Code, recorder.Body.String())
	}
	response := &goipp.Message{}
	if err := response.Decode(bytes.NewReader(recorder.Body.Bytes())); err != nil {
		t.Fatalf("decode proxy response: %v", err)
	}
	return response
}

func acceptanceBasicOperationAttrs(printerURI string) goipp.Attributes {
	return goipp.Attributes{
		goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")),
		iattr.URI("printer-uri", printerURI),
	}
}

func acceptanceURF(width, height, dpi uint32, colorSpace, bitsPerPixel byte) []byte {
	bytesPerPixel := int(bitsPerPixel / 8)
	document := make([]byte, 12+32)
	copy(document[:4], "UNIR")
	document[12] = bitsPerPixel
	document[13] = colorSpace
	binary.BigEndian.PutUint32(document[12+12:12+16], width)
	binary.BigEndian.PutUint32(document[12+16:12+20], height)
	binary.BigEndian.PutUint32(document[12+20:12+24], dpi)
	for row := uint32(0); row < height; row++ {
		document = append(document, 0) // one row; repeat count is zero-based
		for left := width; left > 0; {
			packetPixels := left
			if packetPixels > 128 {
				packetPixels = 128
			}
			// URF literal packets use 257-N. The high bit marks literal data;
			// packets larger than 128 would wrap into a repeat packet.
			document = append(document, byte(257-packetPixels))
			for pixel := uint32(0); pixel < packetPixels; pixel++ {
				for channel := 0; channel < bytesPerPixel; channel++ {
					document = append(document, byte((row+width-left+pixel+uint32(channel))&0xff))
				}
			}
			left -= packetPixels
		}
	}
	return document
}

func acceptanceHasURFToken(attr goipp.Attribute, want string) bool {
	for _, value := range attr.Values {
		text, ok := value.V.(goipp.String)
		if !ok {
			continue
		}
		for _, token := range strings.Split(string(text), ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func minAcceptance(left, right int) int {
	if left < right {
		return left
	}
	return right
}
