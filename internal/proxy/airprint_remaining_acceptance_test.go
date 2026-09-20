package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

func TestAirPrintAcceptanceDEVRGBOnlyMapsAndPreservesPayload(t *testing.T) {
	upstream := newAcceptanceUpstream(t, devRGBOnlyAcceptanceCapabilities())
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "color")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	attributes := acceptanceIPP(t, svc, goipp.OpGetPrinterAttributes, 1001, nil, "", 0, false)
	urf, ok := iattr.Attr(attributes.Printer, "urf-supported")
	if !ok || !acceptanceHasURFToken(urf, "DEVRGB24") || acceptanceHasURFToken(urf, "SRGB24") {
		t.Fatalf("device-RGB client view has wrong URF mapping: %+v", urf)
	}
	if !iattr.HasStringValue(attributes.Printer, "pwg-raster-document-type-supported", "rgb_8") ||
		iattr.HasStringValue(attributes.Printer, "pwg-raster-document-type-supported", "srgb_8") {
		t.Fatalf("device-RGB client view has wrong PWG types: %+v", attributes.Printer)
	}

	document := acceptanceURF(595, 841, 72, 5, 24)
	response := acceptanceIPP(t, svc, goipp.OpPrintJob, 1002, document, "image/urf", 0, false)
	status := goipp.Status(response.Code)
	if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("device-RGB Print-Job status = %s, message=%q", status, statusMessage(response))
	}
	record := upstream.lastOperation(goipp.OpPrintJob)
	if record == nil {
		t.Fatal("upstream did not receive device-RGB Print-Job")
	}
	if got := acceptanceOperationFormat(record.operation); !strings.EqualFold(got, "image/pwg-raster") {
		t.Fatalf("device-RGB upstream document-format = %q, want image/pwg-raster", got)
	}
	if !bytes.HasPrefix(record.payload, []byte("RaS2")) || len(record.payload) < 1800 || !bytes.Equal(record.payload[1800:], document[44:]) {
		t.Fatal("device-RGB translation did not preserve packet payload after the PWG header")
	}

	unsupported := acceptanceIPP(t, svc, goipp.OpPrintJob, 1003, acceptanceURF(595, 841, 72, 1, 24), "image/urf", 0, false)
	if got := goipp.Status(unsupported.Code); got != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("unsupported SRGB document status = %s, want document-format-not-supported", got)
	}
	if got := upstream.countOperation(goipp.OpPrintJob); got != 1 {
		t.Fatalf("unsupported SRGB document reached upstream; Print-Job count = %d, want one successful dispatch", got)
	}
}

func TestAirPrintAcceptanceValidateJobRewritesEmulatedFormat(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	response := acceptanceIPP(t, svc, goipp.OpValidateJob, 1011, nil, "image/urf", 0, false)
	status := goipp.Status(response.Code)
	if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("emulated Validate-Job status = %s, message=%q", status, statusMessage(response))
	}
	record := upstream.lastOperation(goipp.OpValidateJob)
	if record == nil {
		t.Fatal("upstream did not receive Validate-Job")
	}
	if got := acceptanceOperationFormat(record.operation); !strings.EqualFold(got, "image/pwg-raster") {
		t.Fatalf("emulated Validate-Job document-format = %q, want image/pwg-raster", got)
	}
	if len(record.payload) != 0 {
		t.Fatalf("Validate-Job unexpectedly carried %d payload bytes", len(record.payload))
	}
}

func TestAirPrintAcceptanceTransformedUpstreamRejectionPreservesMetadata(t *testing.T) {
	base := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer base.Close()
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()
	svc := newAcceptanceServiceWithStore(t, base, config.AirPrintEmulateIfMissing, "monochrome", jobStore)
	defer svc.Close()
	const rejectionMessage = "printer rejected transformed document"
	svc.upstream.HTTP = &http.Client{Transport: upstreamRejectsOperation(base, goipp.OpPrintJob, goipp.StatusErrorDocumentFormatNotSupported, rejectionMessage)}
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	document := acceptanceURF(595, 841, 72, 0, 8)
	response := acceptanceIPP(t, svc, goipp.OpPrintJob, 1021, document, "image/urf", 0, false)
	if got := goipp.Status(response.Code); got != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("transformed upstream rejection status = %s, want document-format-not-supported", got)
	}
	if response.RequestID != 1021 {
		t.Fatalf("client response request-id = %d, want 1021", response.RequestID)
	}
	if got := statusMessage(response); got != rejectionMessage {
		t.Fatalf("client response status-message = %q, want %q", got, rejectionMessage)
	}

	record := base.lastOperation(goipp.OpPrintJob)
	if record == nil {
		t.Fatal("upstream did not receive the translated Print-Job")
	}
	if got := acceptanceOperationFormat(record.operation); !strings.EqualFold(got, "image/pwg-raster") {
		t.Fatalf("rejected upstream document-format = %q, want image/pwg-raster", got)
	}
	if !bytes.HasPrefix(record.payload, []byte("RaS2")) {
		t.Fatal("rejected upstream request did not contain a staged PWG document")
	}

	jobs, err := jobStore.GetJobs(context.Background(), store.JobFilter{Queue: "office", IncludeTerminal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("durable jobs = %d, want one rejected job", len(jobs))
	}
	job := jobs[0]
	if job.State != store.StateTerminal || job.ObservedState != goipp.StatusErrorDocumentFormatNotSupported.String() || job.ObservedError != rejectionMessage {
		t.Fatalf("rejected transformed job lifecycle = state %q, observed=%q, error=%q", job.State, job.ObservedState, job.ObservedError)
	}
	if job.DocumentFormat != "image/urf" || job.UpstreamDocumentFormat != "image/pwg-raster" || job.RouteKind != store.RouteURFToPWG {
		t.Fatalf("rejected transformed job route metadata = %+v", job)
	}
	if job.PayloadBytes != 0 || job.PageCount != nil || job.DocumentCount != 0 || job.LastDocument {
		t.Fatalf("rejected transformed job retained payload metadata: %+v", job)
	}
}

func TestAirPrintAcceptanceJPEGPassThroughPreservesPayload(t *testing.T) {
	upstreamAttrs := acceptanceRasterCapabilities("monochrome", false, true)
	formats, ok := iattr.Attr(upstreamAttrs, "document-format-supported")
	if !ok {
		t.Fatal("acceptance capabilities omitted document-format-supported")
	}
	formats.Values.Add(goipp.TagMimeType, goipp.String("image/jpeg"))
	upstreamAttrs = iattr.SetAttr(upstreamAttrs, formats)
	upstream := newAcceptanceUpstream(t, upstreamAttrs)
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	document := []byte{0xff, 0xd8, 0x00, 0x10, 0x00, 0xff, 0xda, 0x01, 0x02, 0x03, 0xff, 0xd9}
	response := acceptanceIPP(t, svc, goipp.OpPrintJob, 1031, document, "image/jpeg", 0, false)
	status := goipp.Status(response.Code)
	if status != goipp.StatusOk && status != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("JPEG Print-Job status = %s, message=%q", status, statusMessage(response))
	}
	record := upstream.lastOperation(goipp.OpPrintJob)
	if record == nil {
		t.Fatal("upstream did not receive JPEG Print-Job")
	}
	if got := acceptanceOperationFormat(record.operation); !strings.EqualFold(got, "image/jpeg") {
		t.Fatalf("JPEG upstream document-format = %q, want image/jpeg", got)
	}
	if !bytes.Equal(record.payload, document) {
		t.Fatalf("JPEG pass-through payload changed: got %x, want %x", record.payload, document)
	}
}

func devRGBOnlyAcceptanceCapabilities() goipp.Attributes {
	attrs := acceptanceRasterCapabilities("color", false, false)
	return iattr.SetAttr(attrs, iattr.Keyword("pwg-raster-document-type-supported", "rgb_8"))
}

func upstreamRejectsOperation(base *acceptanceUpstream, operation goipp.Op, status goipp.Status, message string) http.RoundTripper {
	return upstreamRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		decoded := &goipp.Message{}
		if err := decoded.Decode(bytes.NewReader(body)); err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		response, err := base.RoundTrip(request)
		if err != nil || goipp.Op(decoded.Code) != operation {
			return response, err
		}
		_ = response.Body.Close()
		rejected := goipp.NewResponse(decoded.Version, status, decoded.RequestID)
		rejected.Operation = acceptanceResponseOperationAttrs(message)
		envelope, err := rejected.EncodeBytes()
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{goipp.ContentType}},
			Body:       io.NopCloser(bytes.NewReader(envelope)),
			Request:    request,
		}, nil
	})
}
