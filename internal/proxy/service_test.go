package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

func TestPrintJobNormalizesEnvelopeAndStreamsPayload(t *testing.T) {
	var upstreamPayload []byte
	var upstreamJobAttrs goipp.Attributes

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := &goipp.Message{}
		if err := msg.Decode(r.Body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamJobAttrs = msg.Job
		var err error
		upstreamPayload, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}

		resp := goipp.NewResponse(msg.Version, goipp.StatusOk, msg.RequestID)
		resp.Operation = responseOperationAttrs("")
		resp.Job = goipp.Attributes{iattr.Integer("job-id", 77)}
		writeIPP(w, resp)
	}))
	defer upstream.Close()

	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {
				UpstreamURI: strings.Replace(upstream.URL, "http://", "ipp://", 1),
				DisplayName: "Office",
				Policy: config.PolicyConfig{
					Media:          "iso_a4_210x297mm",
					MediaType:      "stationery",
					PrintColorMode: "monochrome",
				},
			},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	reqMsg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 123)
	reqMsg.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
		iattr.Name("requesting-user-name", "jnovak"),
		iattr.Name("job-name", "invoice.pdf"),
	)
	reqMsg.Job = goipp.Attributes{
		iattr.Keyword("media", "na_letter_8.5x11in"),
		iattr.Keyword("print-color-mode", "color"),
		iattr.Keyword("sides", "two-sided-long-edge"),
	}
	envelope, err := reqMsg.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\n1 0 obj << /Type /Page >> endobj\n2 0 obj << /Type /Page >> endobj\n")
	httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", io.MultiReader(bytes.NewReader(envelope), bytes.NewReader(payload)))
	httpReq.Header.Set("content-type", goipp.ContentType)
	rec := httptest.NewRecorder()

	svc.Routes().ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected HTTP status: %d", rec.Code)
	}
	resp := &goipp.Message{}
	if err := resp.Decode(rec.Body); err != nil {
		t.Fatal(err)
	}
	if got, _ := iattr.FirstInt(resp.Job, "job-id"); got != 1 {
		t.Fatalf("expected proxy job-id 1, got %d", got)
	}
	if !bytes.Equal(upstreamPayload, payload) {
		t.Fatalf("payload changed: %q", upstreamPayload)
	}
	if !iattr.HasStringValue(upstreamJobAttrs, "media", "iso_a4_210x297mm") {
		t.Fatal("upstream did not receive forced media")
	}
	if !iattr.HasStringValue(upstreamJobAttrs, "print-color-mode", "monochrome") {
		t.Fatal("upstream did not receive forced color")
	}
	if !iattr.HasStringValue(upstreamJobAttrs, "sides", "two-sided-long-edge") {
		t.Fatal("upstream did not receive preserved sides")
	}

	job, err := jobStore.GetByProxyID(context.Background(), "office", 1)
	if err != nil {
		t.Fatal(err)
	}
	if job.UpstreamJobID != 77 {
		t.Fatalf("unexpected stored job: %+v", job)
	}
	if job.PayloadBytes != int64(len(payload)) {
		t.Fatalf("unexpected payload byte count: %+v", job)
	}
	if job.PageCount == nil || *job.PageCount != 2 {
		t.Fatalf("unexpected page count: %+v", job)
	}
	if job.Copies != 1 {
		t.Fatalf("unexpected copies: %+v", job)
	}
	if job.EstimatedImpressions == nil || *job.EstimatedImpressions != 2 {
		t.Fatalf("unexpected impressions: %+v", job)
	}
}

func TestCountPDFPagesBestEffort(t *testing.T) {
	f, err := os.CreateTemp("", "golieipp-test-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("%PDF\n<< /Type /Pages >>\n<< /Type /Page >>\n<< /Type /Page >>"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	pages, err := countPDFPagesBestEffort(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Fatalf("expected 2 pages, got %d", pages)
	}
}

func TestPayloadRecorderOnlyCountsOctetStreamWhenPayloadLooksLikePDF(t *testing.T) {
	pdfRecorder, err := newPayloadRecorder(strings.NewReader("%PDF-1.7\n<< /Type /Page >>"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, pdfRecorder.Reader()); err != nil {
		t.Fatal(err)
	}
	pdfMeta, err := pdfRecorder.Finish("application/octet-stream", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pdfMeta.PageCount == nil || *pdfMeta.PageCount != 1 {
		t.Fatalf("expected sniffed octet-stream PDF page count, got %+v", pdfMeta)
	}

	rawRecorder, err := newPayloadRecorder(strings.NewReader("not a pdf /Type /Page"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rawRecorder.Reader()); err != nil {
		t.Fatal(err)
	}
	rawMeta, err := rawRecorder.Finish("application/octet-stream", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rawMeta.PageCount != nil {
		t.Fatalf("unexpected page count for non-PDF octet-stream: %+v", rawMeta)
	}
}

func TestCloseJobRewritesProxyJobID(t *testing.T) {
	var upstreamJobID int
	var upstreamJobURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := &goipp.Message{}
		if err := msg.Decode(r.Body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamJobID, _ = iattr.FirstInt(msg.Operation, "job-id")
		upstreamJobURI, _ = iattr.FirstString(msg.Operation, "job-uri")

		resp := goipp.NewResponse(msg.Version, goipp.StatusOk, msg.RequestID)
		resp.Operation = responseOperationAttrs("")
		resp.Job = goipp.Attributes{iattr.Integer("job-id", upstreamJobID)}
		writeIPP(w, resp)
	}))
	defer upstream.Close()

	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()
	proxyID, err := jobStore.CreateJob(context.Background(), store.Job{
		UpstreamJobID: 88,
		Queue:         "office",
		State:         "created",
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: strings.Replace(upstream.URL, "http://", "ipp://", 1)},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	reqMsg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCloseJob, 124)
	reqMsg.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		iattr.Integer("job-id", proxyID),
	)
	envelope, err := reqMsg.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", bytes.NewReader(envelope))
	httpReq.Header.Set("content-type", goipp.ContentType)
	rec := httptest.NewRecorder()

	svc.Routes().ServeHTTP(rec, httpReq)

	if upstreamJobID != 88 {
		t.Fatalf("expected upstream job-id 88, got %d", upstreamJobID)
	}
	if upstreamJobURI != "" {
		t.Fatalf("expected no synthesized upstream job-uri, got %q", upstreamJobURI)
	}
	resp := &goipp.Message{}
	if err := resp.Decode(rec.Body); err != nil {
		t.Fatal(err)
	}
	if got, _ := iattr.FirstInt(resp.Job, "job-id"); got != proxyID {
		t.Fatalf("expected proxy job-id %d in response, got %d", proxyID, got)
	}
}

func TestSendDocumentDoesNotSynthesizeUpstreamJobURI(t *testing.T) {
	var upstreamJobID int
	var upstreamJobURI string
	var upstreamPrinterURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := &goipp.Message{}
		if err := msg.Decode(r.Body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamJobID, _ = iattr.FirstInt(msg.Operation, "job-id")
		upstreamJobURI, _ = iattr.FirstString(msg.Operation, "job-uri")
		upstreamPrinterURI, _ = iattr.FirstString(msg.Operation, "printer-uri")
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}

		resp := goipp.NewResponse(msg.Version, goipp.StatusOk, msg.RequestID)
		resp.Operation = responseOperationAttrs("")
		writeIPP(w, resp)
	}))
	defer upstream.Close()

	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()
	proxyID, err := jobStore.CreateJob(context.Background(), store.Job{
		UpstreamJobID: 99,
		Queue:         "office",
		State:         "created",
	})
	if err != nil {
		t.Fatal(err)
	}

	upstreamURI := strings.Replace(upstream.URL, "http://", "ipp://", 1)
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: upstreamURI},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	reqMsg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSendDocument, 126)
	reqMsg.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		iattr.Integer("job-id", proxyID),
		iattr.URI("job-uri", "ipp://proxy/printers/office/jobs/1"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
		iattr.Boolean("last-document", true),
	)
	envelope, err := reqMsg.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\n1 0 obj << /Type /Page >> endobj\n")
	httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", io.MultiReader(bytes.NewReader(envelope), bytes.NewReader(payload)))
	httpReq.Header.Set("content-type", goipp.ContentType)
	rec := httptest.NewRecorder()

	svc.Routes().ServeHTTP(rec, httpReq)

	if upstreamPrinterURI != upstreamURI {
		t.Fatalf("expected upstream printer-uri %q, got %q", upstreamURI, upstreamPrinterURI)
	}
	if upstreamJobID != 99 {
		t.Fatalf("expected upstream job-id 99, got %d", upstreamJobID)
	}
	if upstreamJobURI != "" {
		t.Fatalf("expected proxy job-uri to be removed, got %q", upstreamJobURI)
	}
}

func TestCreateJobStoresAndReusesUpstreamJobURI(t *testing.T) {
	const realJobURI = "ipp://printer.example/ipp/jobs/123"
	var requests []goipp.Attributes
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := &goipp.Message{}
		if err := msg.Decode(r.Body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		requests = append(requests, msg.Operation.DeepCopy())

		resp := goipp.NewResponse(msg.Version, goipp.StatusOk, msg.RequestID)
		resp.Operation = responseOperationAttrs("")
		resp.Job = goipp.Attributes{
			iattr.Integer("job-id", 123),
			iattr.URI("job-uri", realJobURI),
		}
		writeIPP(w, resp)
	}))
	defer upstream.Close()

	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	upstreamURI := strings.Replace(upstream.URL, "http://", "ipp://", 1)
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: upstreamURI},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	create := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCreateJob, 127)
	create.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		iattr.Name("requesting-user-name", "jnovak"),
		iattr.Name("job-name", "invoice.pdf"),
	)
	createEnvelope, err := create.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/printers/office", bytes.NewReader(createEnvelope))
	createReq.Header.Set("content-type", goipp.ContentType)
	createRec := httptest.NewRecorder()

	svc.Routes().ServeHTTP(createRec, createReq)

	createResp := &goipp.Message{}
	if err := createResp.Decode(createRec.Body); err != nil {
		t.Fatal(err)
	}
	proxyID, _ := iattr.FirstInt(createResp.Job, "job-id")
	job, err := jobStore.GetByProxyID(context.Background(), "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if job.UpstreamJobURI != realJobURI {
		t.Fatalf("expected stored upstream job-uri %q, got %q", realJobURI, job.UpstreamJobURI)
	}

	closeJob := goipp.NewRequest(goipp.DefaultVersion, goipp.OpCloseJob, 128)
	closeJob.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		iattr.Integer("job-id", proxyID),
		iattr.URI("job-uri", "ipp://proxy/printers/office/jobs/1"),
	)
	closeEnvelope, err := closeJob.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	closeReq := httptest.NewRequest(http.MethodPost, "/printers/office", bytes.NewReader(closeEnvelope))
	closeReq.Header.Set("content-type", goipp.ContentType)
	closeRec := httptest.NewRecorder()

	svc.Routes().ServeHTTP(closeRec, closeReq)

	if len(requests) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(requests))
	}
	if got, _ := iattr.FirstString(requests[1], "job-uri"); got != realJobURI {
		t.Fatalf("expected real upstream job-uri %q, got %q", realJobURI, got)
	}
	if got, _ := iattr.FirstInt(requests[1], "job-id"); got != 123 {
		t.Fatalf("expected upstream job-id 123, got %d", got)
	}
}

func TestSendDocumentUpstreamIPPErrorDoesNotUpdatePayloadMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := &goipp.Message{}
		if err := msg.Decode(r.Body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}

		resp := goipp.NewResponse(msg.Version, goipp.StatusErrorDocumentFormatNotSupported, msg.RequestID)
		resp.Operation = responseOperationAttrs("PDF rejected")
		writeIPP(w, resp)
	}))
	defer upstream.Close()

	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()
	proxyID, err := jobStore.CreateJob(context.Background(), store.Job{
		UpstreamJobID: 99,
		Queue:         "office",
		State:         "created",
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: strings.Replace(upstream.URL, "http://", "ipp://", 1)},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	reqMsg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSendDocument, 125)
	reqMsg.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		iattr.Integer("job-id", proxyID),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
		iattr.Boolean("last-document", true),
	)
	envelope, err := reqMsg.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("%PDF-1.7\n1 0 obj << /Type /Page >> endobj\n")
	httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", io.MultiReader(bytes.NewReader(envelope), bytes.NewReader(payload)))
	httpReq.Header.Set("content-type", goipp.ContentType)
	rec := httptest.NewRecorder()

	svc.Routes().ServeHTTP(rec, httpReq)

	resp := &goipp.Message{}
	if err := resp.Decode(rec.Body); err != nil {
		t.Fatal(err)
	}
	if goipp.Status(resp.Code) != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("unexpected response status: %s", goipp.Status(resp.Code))
	}
	job, err := jobStore.GetByProxyID(context.Background(), "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if job.PayloadBytes != 0 || job.PageCount != nil {
		t.Fatalf("rejected payload metadata was stored: %+v", job)
	}
}
