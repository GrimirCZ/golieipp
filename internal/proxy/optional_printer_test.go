package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestRefreshAllSkipsOptionalPrinter(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	svc := newRefreshTestService(t, config.PrinterConfig{
		UpstreamURI: strings.Replace(upstream.URL, "http://", "ipp://", 1),
		Optional:    true,
	})
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("optional printer prevented startup: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("optional printer was probed during blocking startup: %d requests", got)
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
	resp, err := svc.handleGetPrinterAttributes(context.Background(), "office", svc.cfg.Printers["office"], req)
	if err != nil {
		t.Fatal(err)
	}
	if got := goipp.Status(resp.Code); got != goipp.StatusErrorPrinterIsDeactivated {
		t.Fatalf("offline optional printer returned %s", got)
	}
}

func TestRefreshAllFailsForUnavailableRequiredPrinter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	svc := newRefreshTestService(t, config.PrinterConfig{
		UpstreamURI: strings.Replace(upstream.URL, "http://", "ipp://", 1),
	})
	if err := svc.RefreshAll(context.Background()); err == nil {
		t.Fatal("required printer did not prevent startup")
	}
}

func TestRefreshLoopActivatesOptionalPrinterImmediately(t *testing.T) {
	probed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &goipp.Message{}
		if err := req.Decode(r.Body); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad IPP request", http.StatusBadRequest)
			return
		}
		resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
		resp.Operation = responseOperationAttrs("")
		resp.Printer = goipp.Attributes{
			iattr.Keyword("media-supported", "iso_a4_210x297mm"),
			iattr.Keyword("print-color-mode-supported", "monochrome"),
		}
		writeIPP(w, resp)
		select {
		case probed <- struct{}{}:
		default:
		}
	}))
	defer upstream.Close()

	svc := newRefreshTestService(t, config.PrinterConfig{
		UpstreamURI:     strings.Replace(upstream.URL, "http://", "ipp://", 1),
		Optional:        true,
		RefreshInterval: time.Hour,
	})
	if svc.printerAvailable("office") {
		t.Fatal("optional printer was available before probing")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartRefreshLoop(ctx)

	select {
	case <-probed:
	case <-time.After(time.Second):
		t.Fatal("optional printer was not probed immediately")
	}
	deadline := time.Now().Add(time.Second)
	for !svc.printerAvailable("office") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !svc.printerAvailable("office") {
		t.Fatal("optional printer did not become available after a successful probe")
	}
}

func newRefreshTestService(t *testing.T, printer config.PrinterConfig) *Service {
	t.Helper()
	if printer.DisplayName == "" {
		printer.DisplayName = "Office"
	}
	if printer.Policy.Media == "" {
		printer.Policy = config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		}
	}
	cfg := &config.Config{
		Listen:   config.ListenConfig{PublicBaseURL: "ipp://proxy/printers"},
		Printers: map[string]config.PrinterConfig{"office": printer},
	}
	svc, err := NewService(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
