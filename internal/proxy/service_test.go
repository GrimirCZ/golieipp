package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

func activateTestQueue(service *Service, queue string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.capabilities[queue] = goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("operations-supported", goipp.TagEnum,
			goipp.Integer(goipp.OpPrintJob), goipp.Integer(goipp.OpValidateJob),
			goipp.Integer(goipp.OpCreateJob), goipp.Integer(goipp.OpSendDocument),
			goipp.Integer(goipp.OpCancelJob), goipp.Integer(goipp.OpGetJobAttributes),
			goipp.Integer(goipp.OpGetJobs), goipp.Integer(goipp.OpGetPrinterAttributes),
			goipp.Integer(goipp.OpCloseJob), goipp.Integer(goipp.OpIdentifyPrinter)),
		goipp.MakeAttribute("document-format-supported", goipp.TagMimeType, goipp.String("application/pdf")),
	}
	service.queueHealth[queue] = queueHealth{LastSuccess: time.Now().UTC()}
}

func TestReadinessDistinguishesRequiredOptionalAndStaleQueues(t *testing.T) {
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()
	cfg := &config.Config{Printers: map[string]config.PrinterConfig{
		"required": {RefreshInterval: time.Minute},
		"optional": {Optional: true, RefreshInterval: time.Minute},
	}}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	svc.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("inactive required queue returned %d", recorder.Code)
	}

	svc.mu.Lock()
	svc.capabilities["required"] = goipp.Attributes{iattr.Name("printer-name", "Required")}
	svc.queueHealth["required"] = queueHealth{LastSuccess: time.Now().Add(-3 * time.Minute)}
	svc.mu.Unlock()
	recorder = httptest.NewRecorder()
	svc.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stale active queue returned %d", recorder.Code)
	}
	var readiness readinessResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if !readiness.Queues["required"].Stale {
		t.Fatalf("readiness omitted stale state: %+v", readiness)
	}
	if strings.Contains(recorder.Body.String(), "dns_sd_") {
		t.Fatalf("readiness exposed detailed mDNS state: %s", recorder.Body.String())
	}
}

func TestReadinessRedactsUpstreamCredentialsInLastError(t *testing.T) {
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: "ipp://user:secret@printer.local/ipp/print"},
		},
	}
	svc, err := NewService(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.capabilities["office"] = goipp.Attributes{iattr.Name("printer-name", "Office")}
	svc.queueHealth["office"] = queueHealth{
		LastSuccess: time.Now().UTC(),
		LastError:   "request failed for ipp://user:secret@printer.local/ipp/print",
	}
	svc.mu.Unlock()

	recorder := httptest.NewRecorder()
	svc.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("readyz returned %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("readyz leaked upstream credentials: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "ipp://redacted@printer.local/ipp/print") {
		t.Fatalf("readyz did not preserve a redacted diagnostic error: %s", recorder.Body.String())
	}
}

func TestDumpStateIncludesEffectiveConfigAndRedactsUpstreamCredentials(t *testing.T) {
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := &config.Config{
		Listen:   config.ListenConfig{Addr: ":8631", PublicBaseURL: "ipp://proxy.local/printers"},
		Defaults: config.DefaultsConfig{MaxEnvelopeBytes: 12345},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: "ipp://user:secret@printer.local/ipp/print", DisplayName: "Office"},
		},
	}
	svc, err := NewService(cfg, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc.DumpState()

	output := logBuffer.String()
	for _, want := range []string{
		`"section":"application"`,
		`"section":"effective_config"`,
		`"max_envelope_bytes":12345`,
		`"upstream_uri":"ipp://redacted@printer.local/ipp/print"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("state dump missing %q: %s", want, output)
		}
	}
}

func TestClientCapabilitiesDoNotAdvertiseUnsupportedOverrides(t *testing.T) {
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {
				Policy: config.PolicyConfig{
					Media:          "iso_a4_210x297mm",
					PrintColorMode: "monochrome",
				},
			},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	attrs := svc.clientCapabilities("office", cfg.Printers["office"], goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
	})
	if _, ok := iattr.Attr(attrs, "overrides-supported"); ok {
		t.Fatal("client capability view advertises overrides without a supported forwarding path")
	}
}

func TestUpstreamClaimsIPPEverywhereRequiresWellFormedKeywordSet(t *testing.T) {
	tests := []struct {
		name   string
		attrs  goipp.Attributes
		want   bool
		reason string
	}{
		{
			name: "valid claim",
			attrs: goipp.Attributes{goipp.MakeAttr("ipp-features-supported", goipp.TagKeyword,
				goipp.String("ipp-everywhere"), goipp.String("ipp-everywhere-server"))},
			want: true,
		},
		{
			name:   "missing attribute",
			reason: "upstream did not provide ipp-features-supported",
		},
		{
			name: "wrong tag",
			attrs: goipp.Attributes{goipp.MakeAttribute("ipp-features-supported", goipp.TagName,
				goipp.String("ipp-everywhere"))},
			reason: "upstream ipp-features-supported value 0 has tag nameWithoutLanguage, want keyword",
		},
		{
			name:   "empty values",
			attrs:  goipp.Attributes{{Name: "ipp-features-supported"}},
			reason: "upstream provided an empty ipp-features-supported attribute",
		},
		{
			name: "duplicate attributes",
			attrs: goipp.Attributes{
				goipp.MakeAttribute("ipp-features-supported", goipp.TagKeyword, goipp.String("ipp-everywhere")),
				goipp.MakeAttribute("ipp-features-supported", goipp.TagKeyword, goipp.String("ipp-everywhere-server")),
			},
			reason: "upstream provided duplicate ipp-features-supported attributes",
		},
		{
			name:   "missing feature",
			attrs:  goipp.Attributes{goipp.MakeAttribute("ipp-features-supported", goipp.TagKeyword, goipp.String("ipp-everywhere-server"))},
			reason: "upstream ipp-features-supported does not include ipp-everywhere",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, reason := ippEverywhereClaimEligibility(test.attrs)
			if got != test.want {
				t.Fatalf("upstreamClaimsIPPEverywhere() = %t, want %t", got, test.want)
			}
			if reason != test.reason {
				t.Fatalf("ippEverywhereClaimEligibility() reason = %q, want %q", reason, test.reason)
			}
		})
	}
}

func TestRefreshLogsExactIPPEverywhereIneligibilityReason(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	printer := config.PrinterConfig{
		UpstreamURI:       "ipp://printer.invalid/ipp/print",
		IPPEverywhereMode: config.IPPEverywhereAuto,
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	cfg := &config.Config{
		Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{"office": printer},
	}
	svc, err := NewService(cfg, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc.upstream.HTTP = &http.Client{Transport: upstreamRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		request := &goipp.Message{}
		if err := request.Decode(req.Body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		response := goipp.NewResponse(request.Version, goipp.StatusOk, request.RequestID)
		response.Operation = responseOperationAttrs("")
		response.Printer = goipp.Attributes{iattr.Keyword("media-supported", "iso_a4_210x297mm")}
		envelope, err := response.EncodeBytes()
		if err != nil {
			t.Fatalf("encode response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{goipp.ContentType}},
			Body:       io.NopCloser(bytes.NewReader(envelope)),
			Request:    req,
		}, nil
	})}
	if err := svc.refreshOne(context.Background(), "office", printer); err != nil {
		t.Fatalf("refreshOne() returned %v, want ordinary IPP refresh to remain usable", err)
	}

	logText := logs.String()
	for _, want := range []string{
		"level=ERROR",
		"msg=\"printer deemed IPP Everywhere-ineligible\"",
		"queue=office",
		"reason=\"upstream did not provide ipp-features-supported\"",
		"ipp_everywhere_mode=auto",
		"configuration_override=false",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log output missing %q: %s", want, logText)
		}
	}
}

func TestUpstreamSupportsOperationRequiresWellFormedEnumSet(t *testing.T) {
	valid := goipp.MakeAttr("operations-supported", goipp.TagEnum,
		goipp.Integer(goipp.OpGetPrinterAttributes), goipp.Integer(goipp.OpPrintJob))
	tests := []struct {
		name  string
		attrs goipp.Attributes
		want  bool
	}{
		{name: "valid enum", attrs: goipp.Attributes{valid}, want: true},
		{name: "wrong tag", attrs: goipp.Attributes{goipp.MakeAttribute("operations-supported", goipp.TagInteger, goipp.Integer(goipp.OpPrintJob))}},
		{name: "empty values", attrs: goipp.Attributes{{Name: "operations-supported"}}},
		{name: "duplicate attributes", attrs: goipp.Attributes{valid, valid}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := upstreamSupportsOperation(test.attrs, goipp.OpPrintJob); got != test.want {
				t.Fatalf("upstreamSupportsOperation() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSameReconciledJobRequiresStrongIdentity(t *testing.T) {
	attrs := goipp.Attributes{
		iattr.Integer("job-id", 77),
		iattr.Name("job-name", "report.pdf"),
		iattr.Name("job-originating-user-name", "alice"),
	}
	if sameReconciledJob(store.Job{}, attrs) {
		t.Fatal("empty uncertain job matched an upstream job without a strong identity")
	}
	if !sameReconciledJob(store.Job{JobName: "report.pdf"}, attrs) {
		t.Fatal("job-name identity did not match the upstream job")
	}
	if !sameReconciledJob(store.Job{RequestingUser: "alice"}, attrs) {
		t.Fatal("requesting-user identity did not match the upstream job")
	}
}

func TestShapeIPPResponseOrdersUnsupportedBeforeJob(t *testing.T) {
	response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)
	response.Operation = responseOperationAttrs("")
	response.Unsupported = goipp.Attributes{iattr.Keyword("media", "na_letter_8.5x11in")}
	response.Job = goipp.Attributes{iattr.Name("job-name", "report.pdf")}

	shapeIPPResponse(response)
	if response.Groups == nil {
		t.Fatal("response was not converted to canonical groups")
	}
	want := []goipp.Tag{goipp.TagOperationGroup, goipp.TagUnsupportedGroup, goipp.TagJobGroup}
	if len(response.Groups) != len(want) {
		t.Fatalf("group count = %d, want %d: %#v", len(response.Groups), len(want), response.Groups)
	}
	for index, tag := range want {
		if response.Groups[index].Tag != tag {
			t.Fatalf("group %d tag = %s, want %s", index, response.Groups[index].Tag, tag)
		}
	}
	if _, ok := iattr.Attr(response.Groups[1].Attrs, "media"); !ok {
		t.Fatal("unsupported group was not retained")
	}
}

func TestShapeIPPResponsePlacesFidelityUnsupportedBeforeJob(t *testing.T) {
	response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 2)
	response.Operation = responseOperationAttrs("")
	response.Groups = goipp.Groups{
		{Tag: goipp.TagOperationGroup, Attrs: response.Operation},
		{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{iattr.Name("job-name", "report.pdf")}},
	}
	applyNormalizationResult(response, NormalizationResult{
		Substituted: true,
		Unsupported: goipp.Attributes{iattr.Keyword("media", "na_letter_8.5x11in")},
	})

	shapeIPPResponse(response)
	want := []goipp.Tag{goipp.TagOperationGroup, goipp.TagUnsupportedGroup, goipp.TagJobGroup}
	if len(response.Groups) != len(want) {
		t.Fatalf("group count = %d, want %d: %#v", len(response.Groups), len(want), response.Groups)
	}
	for index, tag := range want {
		if response.Groups[index].Tag != tag {
			t.Fatalf("group %d tag = %s, want %s", index, response.Groups[index].Tag, tag)
		}
	}
	if got := len(response.Groups[1].Attrs); got != 1 {
		t.Fatalf("fidelity unsupported attribute count = %d, want 1", got)
	}
}

func TestShapeIPPResponseMergesDuplicateGroups(t *testing.T) {
	response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 3)
	response.Groups = goipp.Groups{
		{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{iattr.Name("job-name", "report.pdf")}},
		{Tag: goipp.TagOperationGroup, Attrs: responseOperationAttrs("")},
		{Tag: goipp.TagPrinterGroup, Attrs: goipp.Attributes{iattr.Name("printer-name", "Office")}},
		{Tag: goipp.TagUnsupportedGroup, Attrs: goipp.Attributes{iattr.Keyword("media", "bad")}},
		{Tag: goipp.TagPrinterGroup, Attrs: goipp.Attributes{iattr.Name("printer-location", "Room 1")}},
		{Tag: goipp.TagUnsupportedGroup, Attrs: goipp.Attributes{iattr.Keyword("sides", "bad")}},
	}

	shapeIPPResponse(response)
	want := []goipp.Tag{goipp.TagOperationGroup, goipp.TagUnsupportedGroup, goipp.TagJobGroup, goipp.TagPrinterGroup}
	if len(response.Groups) != len(want) {
		t.Fatalf("group count = %d, want %d: %#v", len(response.Groups), len(want), response.Groups)
	}
	for index, tag := range want {
		if response.Groups[index].Tag != tag {
			t.Fatalf("group %d tag = %s, want %s", index, response.Groups[index].Tag, tag)
		}
	}
	if len(response.Groups[1].Attrs) != 2 || len(response.Groups[3].Attrs) != 2 {
		t.Fatalf("duplicate groups were not merged: %#v", response.Groups)
	}
}

func TestPrintJobPersistsUncertainAfterCanceledClientContext(t *testing.T) {
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	printer := config.PrinterConfig{
		UpstreamURI: "ipp://upstream.example/ipp/print",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	cfg := &config.Config{
		Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{"office": printer},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	activateTestQueue(svc, "office")

	ctx, cancel := context.WithCancel(context.Background())
	transportCanceled := make(chan struct{})
	svc.upstream.HTTP.Transport = cancelingRoundTripper{
		cancel:   cancel,
		canceled: transportCanceled,
	}
	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 129)
	request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
		iattr.Name("requesting-user-name", "jnovak"),
		iattr.Name("job-name", "canceled.pdf"),
	)

	_, err = svc.handlePrintJob(ctx, "office", printer, request, bytes.NewReader([]byte("document")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upstream request returned %v, want context canceled", err)
	}
	select {
	case <-transportCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream transport did not observe client cancellation")
	}
	job, err := jobStore.GetByProxyID(context.Background(), "office", 1)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.StateUncertain {
		t.Fatalf("canceled upstream request left job in state %q, want uncertain", job.State)
	}
}

func TestRequestedJobAttributesExpandsDescriptionAndTemplate(t *testing.T) {
	operation := goipp.Attributes{goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
		goipp.String("job-description"), goipp.String("job-template"))}
	wanted := requestedJobAttributes(operation)
	attrs := goipp.Attributes{
		iattr.Name("job-name", "report.pdf"),
		iattr.Integer("job-id", 7),
		{Name: "job-state", Values: goipp.Values{{T: goipp.TagEnum, V: goipp.Integer(5)}}},
		iattr.Keyword("job-state-reasons", "none"),
		iattr.Integer("copies", 2),
		iattr.Keyword("media", "iso_a4_210x297mm"),
		iattr.Keyword("x-vendor-only", "secret"),
	}
	projected := projectJobAttributes(attrs, wanted)
	for _, name := range []string{"job-name", "job-id", "job-state", "job-state-reasons", "copies", "media"} {
		if _, ok := iattr.Attr(projected, name); !ok {
			t.Fatalf("requested job category omitted %q: %#v", name, projected)
		}
	}
	if _, ok := iattr.Attr(projected, "x-vendor-only"); ok {
		t.Fatal("job category expansion leaked an unknown attribute")
	}
}

func TestRewriteJobAttrsRepairsMalformedStateAttributes(t *testing.T) {
	attrs := goipp.Attributes{
		iattr.Integer("job-state", 9),
		{Name: "job-state-reasons", Values: goipp.Values{{T: goipp.TagInteger, V: goipp.Integer(1)}}},
	}
	rewritten := rewriteJobAttrs(attrs, 7, "ipp://proxy/printers/office")
	state, ok := iattr.Attr(rewritten, "job-state")
	if !ok || len(state.Values) != 1 || state.Values[0].T != goipp.TagEnum || state.Values[0].V != goipp.Integer(3) {
		t.Fatalf("malformed job-state was not synthesized: %#v", state)
	}
	reasons, ok := iattr.Attr(rewritten, "job-state-reasons")
	if !ok || len(reasons.Values) != 1 || reasons.Values[0].T != goipp.TagKeyword || reasons.Values[0].V != goipp.String("none") {
		t.Fatalf("malformed job-state-reasons was not synthesized: %#v", reasons)
	}
}

func TestCloseCancelsAndWaitsForMaintenanceProbe(t *testing.T) {
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobStore.Close() })

	proxyID, err := jobStore.Reserve(context.Background(), store.Job{
		Queue: "office", RequestingUser: "alice", JobName: "report.pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.MarkUncertain(context.Background(), proxyID, "upstream response was lost"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {UpstreamURI: "ipp://printer.example.test/ipp/print"},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	activateTestQueue(svc, "office")
	probeStarted := make(chan struct{})
	probeCanceled := make(chan struct{})
	allowTransportExit := make(chan struct{})
	svc.upstream.HTTP.Transport = &blockingRoundTripper{
		started:  probeStarted,
		canceled: probeCanceled,
		release:  allowTransportExit,
	}
	releaseTransport := func() {
		select {
		case <-allowTransportExit:
		default:
			close(allowTransportExit)
		}
	}
	t.Cleanup(releaseTransport)

	go svc.StartMaintenanceLoop(context.Background())
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("maintenance probe did not start")
	}

	closed := make(chan error, 1)
	go func() { closed <- svc.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned before the in-flight maintenance probe was canceled")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-probeCanceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the in-flight maintenance probe")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the in-flight maintenance probe finished")
	case <-time.After(50 * time.Millisecond):
	}
	releaseTransport()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for the maintenance loop to finish")
	}
}

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
				Passthrough: config.PassthroughConfig{PreserveJobAttrs: []string{"sides"}},
			},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	activateTestQueue(svc, "office")

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

func TestPrintJobPreservesURFPayloadAndDocumentFormat(t *testing.T) {
	var upstreamFormat string
	var upstreamPayload []byte
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	printer := config.PrinterConfig{
		UpstreamURI: "ipp://printer.invalid/ipp/print",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	cfg := &config.Config{
		Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{"office": printer},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	activateTestQueue(svc, "office")
	svc.mu.Lock()
	svc.capabilities["office"] = iattr.SetAttr(svc.capabilities["office"], goipp.MakeAttr(
		"document-format-supported", goipp.TagMimeType,
		goipp.String("application/pdf"), goipp.String("image/urf"),
	))
	svc.mu.Unlock()
	svc.upstream.HTTP = &http.Client{Transport: upstreamRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		message := &goipp.Message{}
		if err := message.Decode(req.Body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamFormat, _ = iattr.FirstString(message.Operation, "document-format")
		upstreamPayload, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}
		response := goipp.NewResponse(message.Version, goipp.StatusOk, message.RequestID)
		response.Operation = responseOperationAttrs("")
		response.Job = goipp.Attributes{iattr.Integer("job-id", 88)}
		envelope, err := response.EncodeBytes()
		if err != nil {
			t.Fatalf("encode upstream response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{goipp.ContentType}},
			Body:       io.NopCloser(bytes.NewReader(envelope)),
			Request:    req,
		}, nil
	})}

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 124)
	request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("image/urf")),
		iattr.Name("requesting-user-name", "jnovak"),
		iattr.Name("job-name", "raster.urf"),
	)
	payload := []byte("URF\x00\x01\x02opaque-raster-bytes")
	response, err := svc.handlePrintJob(context.Background(), "office", printer, request, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if goipp.Status(response.Code) != goipp.StatusOk {
		t.Fatalf("upstream response status = %s", goipp.Status(response.Code))
	}
	if upstreamFormat != "image/urf" {
		t.Fatalf("upstream document format = %q, want image/urf", upstreamFormat)
	}
	if !bytes.Equal(upstreamPayload, payload) {
		t.Fatalf("URF payload changed: %q", upstreamPayload)
	}
	job, err := jobStore.GetByProxyID(context.Background(), "office", 1)
	if err != nil {
		t.Fatal(err)
	}
	if job.DocumentFormat != "image/urf" || job.PayloadBytes != int64(len(payload)) {
		t.Fatalf("URF job metadata = %+v", job)
	}
}

func TestUnknownLengthPrintJobStagesDocumentBeforeUpstream(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamPayload := make(chan []byte, 1)
	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
		default:
			close(upstreamStarted)
		}
		request := &goipp.Message{}
		if err := request.Decode(r.Body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream payload: %v", err)
			return
		}
		upstreamPayload <- payload
		response := goipp.NewResponse(request.Version, goipp.StatusOk, request.RequestID)
		response.Operation = responseOperationAttrs("")
		response.Job = goipp.Attributes{iattr.Integer("job-id", 81)}
		writeIPP(w, response)
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
			"office": {
				UpstreamURI: upstreamURI,
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
	activateTestQueue(svc, "office")

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 401)
	request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
		iattr.Name("requesting-user-name", "alice"),
		iattr.Name("job-name", "staged.pdf"),
	)
	envelope, err := request.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("document bytes")
	body := newGatedRequestBody(append(append([]byte{}, envelope...), payload...))
	httpRequest := httptest.NewRequest(http.MethodPost, "/printers/office", body)
	httpRequest.ContentLength = -1
	httpRequest.Header.Set("content-type", goipp.ContentType)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		svc.Routes().ServeHTTP(recorder, httpRequest)
		close(done)
	}()

	select {
	case <-body.exhausted:
	case <-time.After(time.Second):
		t.Fatal("request body was not read through the document before staging")
	}
	select {
	case <-upstreamStarted:
		t.Fatal("upstream received a request before unknown-length body reached EOF")
	default:
	}
	close(body.release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("staged Print-Job did not complete")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected HTTP status: %d", recorder.Code)
	}
	select {
	case got := <-upstreamPayload:
		if !bytes.Equal(got, payload) {
			t.Fatalf("upstream payload = %q, want %q", got, payload)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the staged payload")
	}
}

func TestUnknownLengthPrintJobRejectsOversizeBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	upstreamURI := strings.Replace(upstream.URL, "http://", "ipp://", 1)
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{MaxDocumentBytes: 4},
		Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{"office": {UpstreamURI: upstreamURI}},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	activateTestQueue(svc, "office")

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 402)
	request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/octet-stream")),
	)
	envelope, err := request.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	body := io.NopCloser(bytes.NewReader(append(envelope, []byte("12345")...)))
	httpRequest := httptest.NewRequest(http.MethodPost, "/printers/office", body)
	httpRequest.ContentLength = -1
	httpRequest.Header.Set("content-type", goipp.ContentType)
	recorder := httptest.NewRecorder()
	svc.Routes().ServeHTTP(recorder, httpRequest)

	response := &goipp.Message{}
	if err := response.Decode(recorder.Body); err != nil {
		t.Fatal(err)
	}
	if got := goipp.Status(response.Code); got != goipp.StatusErrorRequestEntity {
		t.Fatalf("oversize unknown-length request returned %s, want %s", got, goipp.StatusErrorRequestEntity)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("oversize unknown-length request reached upstream %d times", got)
	}
}

func TestCanceledUnknownLengthPrintJobStopsStagingAndCleansTempFile(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	jobStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer jobStore.Close()

	upstreamURI := strings.Replace(upstream.URL, "http://", "ipp://", 1)
	cfg := &config.Config{
		Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{"office": {UpstreamURI: upstreamURI}},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	activateTestQueue(svc, "office")

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 403)
	request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/octet-stream")),
	)
	envelope, err := request.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	body := newGatedRequestBody(append(append([]byte{}, envelope...), []byte("document")...))
	httpRequest := httptest.NewRequest(http.MethodPost, "/printers/office", body)
	httpRequest.ContentLength = -1
	httpRequest.Header.Set("content-type", goipp.ContentType)
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	httpRequest = httpRequest.WithContext(requestContext)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		svc.Routes().ServeHTTP(recorder, httpRequest)
		close(done)
	}()
	select {
	case <-body.exhausted:
	case <-time.After(time.Second):
		t.Fatal("request body was not staged")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceling unknown-length staging did not stop the handler")
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("canceled unknown-length request reached upstream %d times", got)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging left temporary files behind: %v", entries)
	}
}

func TestUnknownLengthDocumentDefersStagingUntilRequestGatesPass(t *testing.T) {
	tests := []struct {
		name        string
		activate    bool
		unsupported bool
		invalid     bool
	}{
		{name: "invalid protocol", activate: true, invalid: true},
		{name: "inactive queue"},
		{name: "unsupported operation", activate: true, unsupported: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobStore, err := store.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer jobStore.Close()

			cfg := &config.Config{
				Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
				Printers: map[string]config.PrinterConfig{"office": {}},
			}
			svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if test.activate {
				activateTestQueue(svc, "office")
			}
			if test.unsupported {
				svc.mu.Lock()
				svc.capabilities["office"] = goipp.Attributes{
					iattr.Keyword("media-supported", "iso_a4_210x297mm"),
					goipp.MakeAttr("operations-supported", goipp.TagEnum, goipp.Integer(goipp.OpGetPrinterAttributes)),
				}
				svc.mu.Unlock()
			}

			request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 404)
			request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
				goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/octet-stream")),
			)
			if test.invalid {
				request.Job = goipp.Attributes{
					iattr.Keyword("media", "iso_a4_210x297mm"),
					goipp.MakeAttrCollection("media-col", iattr.Keyword("media-size-name", "iso_a4_210x297mm")),
				}
			}
			envelope, err := request.EncodeBytes()
			if err != nil {
				t.Fatal(err)
			}
			body := newGatedRequestBody(append(append([]byte{}, envelope...), []byte("document")...))
			httpRequest := httptest.NewRequest(http.MethodPost, "/printers/office", body)
			httpRequest.ContentLength = -1
			httpRequest.Header.Set("content-type", goipp.ContentType)
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				svc.Routes().ServeHTTP(recorder, httpRequest)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(time.Second):
				body.Close()
				t.Fatal("request gate waited for full unknown-length document staging")
			}
			body.Close()
		})
	}
}

func newIPv4TestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return server
}

type gatedRequestBody struct {
	data      []byte
	exhausted chan struct{}
	release   chan struct{}
	closed    chan struct{}
}

func newGatedRequestBody(data []byte) *gatedRequestBody {
	return &gatedRequestBody{
		data:      data,
		exhausted: make(chan struct{}),
		release:   make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

func (b *gatedRequestBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		if len(b.data) == 0 {
			select {
			case <-b.exhausted:
			default:
				close(b.exhausted)
			}
		}
		return n, nil
	}
	select {
	case <-b.release:
		return 0, io.EOF
	case <-b.closed:
		return 0, errors.New("request body closed")
	}
}

func (b *gatedRequestBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestRequestIDZeroIsEchoedAndAccepted(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
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
			"office": {UpstreamURI: strings.Replace(upstream.URL, "http://", "ipp://", 1)},
		},
	}
	svc, err := NewService(cfg, jobStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	activateTestQueue(svc, "office")
	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 0)
	request.Operation = iattr.BasicOperationAttrs("ipp://proxy/printers/office")
	envelope, err := request.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", bytes.NewReader(envelope))
	httpReq.Header.Set("content-type", goipp.ContentType)
	rec := httptest.NewRecorder()
	svc.Routes().ServeHTTP(rec, httpReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected HTTP status: %d", rec.Code)
	}
	response := &goipp.Message{}
	if err := response.Decode(rec.Body); err != nil {
		t.Fatal(err)
	}
	if goipp.Status(response.Code) != goipp.StatusOk {
		t.Fatalf("unexpected IPP status: %s", goipp.Status(response.Code))
	}
	if response.RequestID != 0 {
		t.Fatalf("response did not echo request-id 0: %d", response.RequestID)
	}
	if _, ok := iattr.Attr(response.Printer, "printer-uri-supported"); !ok {
		t.Fatal("valid request-id zero response omitted printer attributes")
	}
	if upstreamCalls != 0 {
		t.Fatalf("bad request was forwarded upstream %d times", upstreamCalls)
	}
}

func TestValidateRequiredOperationAttrs(t *testing.T) {
	valid := iattr.BasicOperationAttrs("ipp://proxy/printers/office")
	tests := []struct {
		name  string
		attrs goipp.Attributes
		op    goipp.Op
		valid bool
	}{
		{name: "valid", attrs: valid, op: goipp.OpGetPrinterAttributes, valid: true},
		{name: "none", attrs: nil, op: goipp.OpGetPrinterAttributes},
		{name: "missing natural language", attrs: valid[:1], op: goipp.OpGetPrinterAttributes},
		{name: "missing charset", attrs: valid[1:], op: goipp.OpGetPrinterAttributes},
		{name: "reversed required attributes", attrs: goipp.Attributes{valid[1], valid[0], valid[2]}, op: goipp.OpGetPrinterAttributes},
		{name: "missing printer uri", attrs: valid[:2], op: goipp.OpGetPrinterAttributes},
		{name: "job operation does not require printer uri here", attrs: valid[:2], op: goipp.OpCancelJob, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRequiredOperationAttrs(test.attrs, test.op)
			if test.valid && err != nil {
				t.Fatalf("valid operation attributes rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid operation attributes accepted")
			}
		})
	}
}

func TestSupportedIPPVersion(t *testing.T) {
	for _, version := range []goipp.Version{
		goipp.MakeVersion(1, 0),
		goipp.MakeVersion(1, 1),
		goipp.MakeVersion(2, 0),
	} {
		if !supportedIPPVersion(version) {
			t.Fatalf("supported IPP version rejected: %s", version)
		}
	}
	if supportedIPPVersion(goipp.MakeVersion(0, 0)) {
		t.Fatal("IPP version 0.0 was accepted")
	}
}

func TestCancelMyJobsIsEmulatedAndIdentifyPrinterIsForwarded(t *testing.T) {
	var operations []goipp.Op
	var printerURIs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := &goipp.Message{}
		if err := request.Decode(r.Body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		operations = append(operations, goipp.Op(request.Code))
		uri, _ := iattr.FirstString(request.Operation, "printer-uri")
		printerURIs = append(printerURIs, uri)
		response := goipp.NewResponse(request.Version, goipp.StatusOk, request.RequestID)
		response.Operation = responseOperationAttrs("")
		writeIPP(w, response)
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

	activateTestQueue(svc, "office")
	requests := []struct {
		op   goipp.Op
		attr goipp.Attribute
	}{
		{goipp.OpCancelMyJobs, iattr.Name("requesting-user-name", "jnovak")},
		{goipp.OpIdentifyPrinter, iattr.Keyword("identify-actions", "flash")},
	}
	for index, testRequest := range requests {
		request := goipp.NewRequest(goipp.DefaultVersion, testRequest.op, uint32(301+index))
		request.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"), testRequest.attr)
		envelope, err := request.EncodeBytes()
		if err != nil {
			t.Fatal(err)
		}
		httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", bytes.NewReader(envelope))
		httpReq.Header.Set("content-type", goipp.ContentType)
		rec := httptest.NewRecorder()
		svc.Routes().ServeHTTP(rec, httpReq)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned unexpected HTTP status: %d", testRequest.op, rec.Code)
		}
		response := &goipp.Message{}
		if err := response.Decode(rec.Body); err != nil {
			t.Fatalf("decode %s response: %v", testRequest.op, err)
		}
		if goipp.Status(response.Code) != goipp.StatusOk {
			t.Fatalf("%s returned unexpected IPP status: %s", testRequest.op, goipp.Status(response.Code))
		}
	}

	if len(operations) != 1 {
		t.Fatalf("expected only Identify-Printer upstream, got %v", operations)
	}
	if operations[0] != goipp.OpIdentifyPrinter || printerURIs[0] != upstreamURI {
		t.Fatalf("Identify-Printer forwarding mismatch: %v %v", operations, printerURIs)
	}
}

func TestPrintJobForwardsAllowedMediaAndFallsBackToDefault(t *testing.T) {
	var upstreamMedia []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := &goipp.Message{}
		if err := msg.Decode(r.Body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read upstream payload: %v", err)
		}
		media, _ := iattr.FirstString(msg.Job, "media")
		upstreamMedia = append(upstreamMedia, media)
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

	upstreamURI := strings.Replace(upstream.URL, "http://", "ipp://", 1)
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": {
				UpstreamURI: upstreamURI,
				Policy: config.PolicyConfig{
					MediaSupported: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"},
					MediaDefault:   "iso_a4_210x297mm",
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

	activateTestQueue(svc, "office")
	printJob := func(requestID uint32, media string) {
		t.Helper()
		reqMsg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, requestID)
		reqMsg.Operation = append(iattr.BasicOperationAttrs("ipp://proxy/printers/office"),
			goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/octet-stream")),
			iattr.Name("requesting-user-name", "jnovak"),
			iattr.Name("job-name", "media-test"),
		)
		reqMsg.Job = goipp.Attributes{iattr.Keyword("media", media)}
		envelope, err := reqMsg.EncodeBytes()
		if err != nil {
			t.Fatal(err)
		}
		httpReq := httptest.NewRequest(http.MethodPost, "/printers/office", io.MultiReader(bytes.NewReader(envelope), strings.NewReader("payload")))
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
		if got := goipp.Status(resp.Code); got >= goipp.StatusErrorBadRequest {
			t.Fatalf("unexpected IPP status: %s", got)
		}
	}

	printJob(201, "na_letter_8.5x11in")
	printJob(202, "jis_b5_182x257mm")
	if len(upstreamMedia) != 2 {
		t.Fatalf("expected two upstream requests, got %d", len(upstreamMedia))
	}
	if upstreamMedia[0] != "na_letter_8.5x11in" {
		t.Fatalf("allowed non-default media was not forwarded: %q", upstreamMedia[0])
	}
	if upstreamMedia[1] != "iso_a4_210x297mm" {
		t.Fatalf("unsupported media did not fall back to default: %q", upstreamMedia[1])
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

	activateTestQueue(svc, "office")
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

func TestSendDocumentUsesPrinterURIAndJobIDOnly(t *testing.T) {
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
		UpstreamJobID:  99,
		UpstreamJobURI: "ipp://printer.example/ipp/print/jobs/99",
		Queue:          "office",
		State:          "created",
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

	activateTestQueue(svc, "office")
	reqMsg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSendDocument, 126)
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

	if upstreamPrinterURI != upstreamURI {
		t.Fatalf("expected upstream printer-uri %q, got %q", upstreamURI, upstreamPrinterURI)
	}
	if upstreamJobID != 99 {
		t.Fatalf("expected upstream job-id 99, got %d", upstreamJobID)
	}
	if upstreamJobURI != "" {
		t.Fatalf("expected proxy job-uri to be removed, got %q", upstreamJobURI)
	}
	job, err := jobStore.GetByProxyID(context.Background(), "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if job.DocumentCount != 1 || !job.LastDocument {
		t.Fatalf("accepted document did not update single-document state: %+v", job)
	}
}

func TestCreateJobStoresUpstreamJobURIButUsesPrinterURIAndJobID(t *testing.T) {
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

	activateTestQueue(svc, "office")
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
	if got, _ := iattr.FirstString(requests[1], "job-uri"); got != "" {
		t.Fatalf("expected job-uri to be removed, got %q", got)
	}
	if got, _ := iattr.FirstInt(requests[1], "job-id"); got != 123 {
		t.Fatalf("expected upstream job-id 123, got %d", got)
	}
	if got, _ := iattr.FirstString(requests[1], "printer-uri"); got != upstreamURI {
		t.Fatalf("expected upstream printer-uri %q, got %q", upstreamURI, got)
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

	activateTestQueue(svc, "office")
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
	if job.State != "failed" {
		t.Fatalf("rejected document left job state %q, expected failed", job.State)
	}
}

type cancelingRoundTripper struct {
	cancel   context.CancelFunc
	canceled chan<- struct{}
}

func (t cancelingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.cancel()
	close(t.canceled)
	return nil, req.Context().Err()
}
