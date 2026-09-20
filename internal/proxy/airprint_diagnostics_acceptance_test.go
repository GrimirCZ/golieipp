package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

// These checks exercise the diagnostic snapshot after a committed capability
// refresh. They intentionally use the in-process acceptance upstream from
// airprint_acceptance_test.go, so the assertions cover the same route that a
// client would use without opening a listener.
func TestAirPrintDiagnosticsAcceptanceReportsEmulationRoute(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	section := acceptanceApplicationDiagnosticSection(t, svc)
	queue := acceptanceDiagnosticQueue(t, section, "office")
	if got := fmt.Sprint(queue["airprint_path"]); got != string(AirPrintPathEmulated) {
		t.Fatalf("airprint_path = %q, want %q", got, AirPrintPathEmulated)
	}
	if got := fmt.Sprint(queue["airprint_reason"]); got == "" {
		t.Fatal("airprint_reason is empty")
	}

	tokens, ok := queue["synthesized_urf"].([]string)
	if !ok {
		t.Fatalf("synthesized_urf has type %T, want []string", queue["synthesized_urf"])
	}
	for _, want := range []string{"V1.4", "W8", "RS72"} {
		if !containsDiagnosticString(tokens, want) {
			t.Fatalf("synthesized_urf = %v, missing %q", tokens, want)
		}
	}

	routes, ok := queue["document_routes"].([]map[string]any)
	if !ok || len(routes) == 0 {
		t.Fatalf("document_routes = %#v, want at least one route", queue["document_routes"])
	}
	var route map[string]any
	for _, candidate := range routes {
		if candidate["client_format"] == "image/urf" {
			route = candidate
			break
		}
	}
	if route == nil {
		t.Fatalf("document_routes = %#v, missing image/urf route", routes)
	}
	for key, want := range map[string]any{
		"client_format":   "image/urf",
		"upstream_format": "image/pwg-raster",
		"transform":       true,
		"mapping":         "W8->sgray_8",
		"media":           "iso_a4_210x297mm",
		"resolution_x":    uint32(72),
		"resolution_y":    uint32(72),
		"sheet_back":      "normal",
	} {
		if got := route[key]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("route[%q] = %#v, want %#v", key, got, want)
		}
	}
}

func TestAirPrintDiagnosticsAcceptanceRedactsTranslationError(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	// Keep the error and configured URI identical to exercise the same
	// credential-redaction path used for probe failures.
	const upstreamURI = "ipp://alice:secret@example.test/ipp/print"
	svc.mu.Lock()
	printer := svc.cfg.Printers["office"]
	printer.UpstreamURI = upstreamURI
	svc.cfg.Printers["office"] = printer
	svc.mu.Unlock()
	svc.recordTranslationError("office", errors.New("translation failed while reading "+upstreamURI))

	section := acceptanceApplicationDiagnosticSection(t, svc)
	queue := acceptanceDiagnosticQueue(t, section, "office")
	lastError, ok := queue["last_translation_error"].(string)
	if !ok || lastError == "" {
		t.Fatalf("last_translation_error = %#v, want redacted error", queue["last_translation_error"])
	}
	if strings.Contains(lastError, "secret") || strings.Contains(lastError, "alice:") {
		t.Fatalf("translation error leaked credentials: %q", lastError)
	}
	if !strings.Contains(lastError, "redacted") {
		t.Fatalf("translation error was not visibly redacted: %q", lastError)
	}
}

func TestAirPrintDiagnosticsAcceptanceRecordsSendDocumentTranslationError(t *testing.T) {
	upstream := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	defer upstream.Close()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	defer svc.Close()
	if err := svc.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll() = %v", err)
	}

	create := acceptanceIPP(t, svc, goipp.OpCreateJob, 61, nil, "image/urf", 0, false)
	proxyJobID, ok := iattr.FirstInt(create.Job, "job-id")
	if !ok || proxyJobID < 1 {
		t.Fatalf("Create-Job did not return a proxy job-id: %+v", create.Job)
	}
	send := acceptanceIPP(t, svc, goipp.OpSendDocument, 62, []byte("UNIR\x00bad"), "image/urf", proxyJobID, true)
	if status := goipp.Status(send.Code); status == goipp.StatusOk || status == goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("malformed Send-Document unexpectedly succeeded: %s", status)
	}
	if got := upstream.countOperation(goipp.OpSendDocument); got != 0 {
		t.Fatalf("malformed Send-Document reached upstream %d times", got)
	}

	section := acceptanceApplicationDiagnosticSection(t, svc)
	queue := acceptanceDiagnosticQueue(t, section, "office")
	lastError, ok := queue["last_translation_error"].(string)
	if !ok || lastError == "" {
		t.Fatalf("last_translation_error = %#v, want Send-Document failure", queue["last_translation_error"])
	}
}

func acceptanceApplicationDiagnosticSection(t *testing.T, svc *Service) map[string]any {
	t.Helper()
	sections, err := (applicationDiagnosticPlugin{}).Dump(context.Background(), svc.captureDiagnosticSnapshot())
	if err != nil {
		t.Fatalf("application diagnostics: %v", err)
	}
	for _, section := range sections {
		if section.Name != "queue" {
			continue
		}
		if queue, ok := section.Value.(map[string]any); ok && queue["queue"] == "office" {
			return queue
		}
	}
	t.Fatalf("office queue diagnostic section was not emitted: %#v", sections)
	return nil
}

func acceptanceDiagnosticQueue(t *testing.T, section map[string]any, queueName string) map[string]any {
	t.Helper()
	if section == nil || section["queue"] != queueName {
		t.Fatalf("diagnostic queue = %#v, want %q", section["queue"], queueName)
	}
	return section
}

func containsDiagnosticString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
