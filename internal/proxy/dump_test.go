package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestDumpPrinterCapabilitiesIncludesRawUpstreamMediaAndAttemptsEveryQueue(t *testing.T) {
	probed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := &goipp.Message{}
		if err := request.Decode(r.Body); err != nil {
			t.Errorf("decode probe request: %v", err)
			return
		}
		response := goipp.NewResponse(request.Version, goipp.StatusOk, request.RequestID)
		response.Operation = responseOperationAttrs("")
		response.Printer = goipp.Attributes{
			iattr.Keywords("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in"),
			iattr.Keyword("printer-info", "raw printer"),
			iattr.Keyword("job-password", "should-not-leak"),
		}
		writeIPP(w, response)
		select {
		case probed <- struct{}{}:
		default:
		}
	}))
	defer upstream.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	goodURI := strings.Replace(upstream.URL, "http://", "ipp://", 1)
	badURI := strings.Replace(bad.URL, "http://", "ipp://", 1)
	cfg := &config.Config{Printers: map[string]config.PrinterConfig{
		"z-office": {UpstreamURI: badURI, Optional: true},
		"a-office": {UpstreamURI: goodURI},
	}}
	var output bytes.Buffer
	err := DumpPrinterCapabilities(context.Background(), cfg, &output)
	if err == nil {
		t.Fatal("expected error for failed queue")
	}
	var report CapabilityDump
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode capability report: %v\n%s", decodeErr, output.String())
	}
	if len(report.Printers) != 2 || report.Printers[0].Queue != "a-office" || report.Printers[1].Queue != "z-office" {
		t.Fatalf("queues were not deterministic: %+v", report.Printers)
	}
	if len(report.Printers[0].Attributes) == 0 {
		t.Fatal("raw upstream attributes missing")
	}
	mediaFound := false
	passwordLeaked := false
	for _, attr := range report.Printers[0].Attributes {
		if attr.Name == "media-supported" && strings.Contains(strings.Join(fmtAny(attr.Values), ","), "na_letter_8.5x11in") {
			mediaFound = true
		}
		if attr.Name == "job-password" && strings.Contains(strings.Join(fmtAny(attr.Values), ","), "should-not-leak") {
			passwordLeaked = true
		}
	}
	if !mediaFound {
		t.Fatal("upstream-only media value missing from raw dump")
	}
	if passwordLeaked {
		t.Fatal("sensitive attribute value leaked")
	}
	if report.Printers[1].Error == "" {
		t.Fatal("failed queue did not include an error")
	}
	select {
	case <-probed:
	default:
		t.Fatal("successful queue was not probed")
	}
}

func fmtAny(values []any) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = fmt.Sprint(value)
	}
	return out
}
