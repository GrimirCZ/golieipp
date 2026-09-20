package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

// These tests exercise the emulated route after the ordinary HTTP gates have
// admitted the request. They prove that translation failures are still fully
// staged and rejected before the fake upstream sees a document.
func TestAirPrintFailureAcceptanceCancellationDoesNotDispatchOrLeakStaging(t *testing.T) {
	for _, operation := range []goipp.Op{goipp.OpPrintJob, goipp.OpSendDocument} {
		t.Run(operation.String(), func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("TMPDIR", tempDir)
			upstream := newAcceptanceUpstream(t, smallFailureAcceptanceCapabilities())
			defer upstream.Close()
			svc := newSmallFailureAcceptanceService(t, upstream)
			defer svc.Close()
			if err := svc.RefreshAll(context.Background()); err != nil {
				t.Fatalf("RefreshAll() = %v", err)
			}

			var jobID int
			if operation == goipp.OpSendDocument {
				created := acceptanceIPP(t, svc, goipp.OpCreateJob, 901, nil, "image/urf", 0, false)
				var ok bool
				jobID, ok = iattr.FirstInt(created.Job, "job-id")
				if !ok || jobID < 1 {
					t.Fatalf("Create-Job response has no proxy job-id: %+v", created.Job)
				}
			}

			document := acceptanceURF(16, 16, 72, 0, 8)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response := acceptanceIPPWithContext(t, svc, ctx, operation, 902, document, "image/urf", jobID, true, func(envelope []byte) io.Reader {
				return io.MultiReader(bytes.NewReader(envelope), &acceptanceCancelAfterRead{
					reader: bytes.NewReader(document),
					cancel: cancel,
				})
			})
			if status := goipp.Status(response.Code); status != goipp.StatusErrorServiceUnavailable {
				t.Fatalf("canceled %s status = %s, want service-unavailable", operation, status)
			}
			if got := upstream.countOperation(operation); got != 0 {
				t.Fatalf("canceled %s reached upstream %d times", operation, got)
			}
			assertAirPrintTempDirEmpty(t, tempDir)
		})
	}
}

func TestAirPrintFailureAcceptanceConfiguredLimitDoesNotDispatchOrLeakStaging(t *testing.T) {
	for _, operation := range []goipp.Op{goipp.OpPrintJob, goipp.OpSendDocument} {
		t.Run(operation.String(), func(t *testing.T) {
			tempDir := t.TempDir()
			t.Setenv("TMPDIR", tempDir)
			upstream := newAcceptanceUpstream(t, smallFailureAcceptanceCapabilities())
			defer upstream.Close()
			svc := newSmallFailureAcceptanceService(t, upstream)
			defer svc.Close()
			// The input fits under the request limit, while the PWG header and
			// emitted row exceed the translator's output limit. This reaches
			// emulated translation instead of the HTTP envelope guard.
			svc.cfg.Defaults.MaxEnvelopeBytes = 512
			svc.cfg.Defaults.MaxDocumentBytes = 1024
			if err := svc.RefreshAll(context.Background()); err != nil {
				t.Fatalf("RefreshAll() = %v", err)
			}

			var jobID int
			if operation == goipp.OpSendDocument {
				created := acceptanceIPP(t, svc, goipp.OpCreateJob, 911, nil, "image/urf", 0, false)
				var ok bool
				jobID, ok = iattr.FirstInt(created.Job, "job-id")
				if !ok || jobID < 1 {
					t.Fatalf("Create-Job response has no proxy job-id: %+v", created.Job)
				}
			}

			document := acceptanceURF(16, 16, 72, 0, 8)
			response := acceptanceIPP(t, svc, operation, 912, document, "image/urf", jobID, true)
			if status := goipp.Status(response.Code); status != goipp.StatusErrorRequestEntity {
				t.Fatalf("limited %s status = %s, want request-entity", operation, status)
			}
			if got := upstream.countOperation(operation); got != 0 {
				t.Fatalf("limited %s reached upstream %d times", operation, got)
			}
			assertAirPrintTempDirEmpty(t, tempDir)
		})
	}
}

func TestAirPrintFailureAcceptanceTempStagingFailureDoesNotDispatch(t *testing.T) {
	for _, operation := range []goipp.Op{goipp.OpPrintJob, goipp.OpSendDocument} {
		t.Run(operation.String(), func(t *testing.T) {
			tempDir := t.TempDir()
			badTempPath := filepath.Join(tempDir, "not-a-directory")
			if err := os.WriteFile(badTempPath, []byte("regular file"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", badTempPath)
			upstream := newAcceptanceUpstream(t, smallFailureAcceptanceCapabilities())
			defer upstream.Close()
			svc := newSmallFailureAcceptanceService(t, upstream)
			defer svc.Close()
			if err := svc.RefreshAll(context.Background()); err != nil {
				t.Fatalf("RefreshAll() = %v", err)
			}

			var jobID int
			if operation == goipp.OpSendDocument {
				created := acceptanceIPP(t, svc, goipp.OpCreateJob, 921, nil, "image/urf", 0, false)
				var ok bool
				jobID, ok = iattr.FirstInt(created.Job, "job-id")
				if !ok || jobID < 1 {
					t.Fatalf("Create-Job response has no proxy job-id: %+v", created.Job)
				}
			}

			document := acceptanceURF(16, 16, 72, 0, 8)
			response := acceptanceIPP(t, svc, operation, 922, document, "image/urf", jobID, true)
			if status := goipp.Status(response.Code); status == goipp.StatusOk || status == goipp.StatusOkIgnoredOrSubstituted {
				t.Fatalf("temp-failing %s unexpectedly succeeded: %s", operation, status)
			}
			if got := upstream.countOperation(operation); got != 0 {
				t.Fatalf("temp-failing %s reached upstream %d times", operation, got)
			}
			entries, err := os.ReadDir(tempDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(badTempPath) {
				t.Fatalf("temp staging failure changed temp directory: %v", entries)
			}
		})
	}
}

type acceptanceCancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *acceptanceCancelAfterRead) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	if r.reads > 0 {
		r.cancel()
	}
	r.reads++
	return r.reader.Read(p)
}

func acceptanceIPPWithContext(t *testing.T, svc *Service, ctx context.Context, op goipp.Op, requestID uint32, document []byte, format string, jobID int, lastDocument bool, body func([]byte) io.Reader) *goipp.Message {
	t.Helper()
	request := goipp.NewRequest(goipp.DefaultVersion, op, requestID)
	request.Operation = append(request.Operation, acceptanceBasicOperationAttrs("ipp://proxy.example:631/printers/office")...)
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
	envelope, err := request.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/printers/office", io.NopCloser(body(envelope))).WithContext(ctx)
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

func assertAirPrintTempDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, strings.TrimSpace(entry.Name()))
		}
		t.Fatalf("translation left temporary files behind: %v", names)
	}
}

const smallFailureMedia = "custom_5.65x5.65mm"

func smallFailureAcceptanceCapabilities() goipp.Attributes {
	attrs := acceptanceRasterCapabilities("monochrome", false, true)
	attrs = iattr.SetAttr(attrs, iattr.Keyword("media-supported", smallFailureMedia))
	attrs = iattr.SetAttr(attrs, iattr.Keyword("media-default", smallFailureMedia))
	attrs = iattr.SetAttr(attrs, iattr.Keyword("media-ready", smallFailureMedia))
	return attrs
}

func newSmallFailureAcceptanceService(t *testing.T, upstream *acceptanceUpstream) *Service {
	t.Helper()
	svc := newAcceptanceService(t, upstream, config.AirPrintEmulateIfMissing, "monochrome")
	printer := svc.cfg.Printers["office"]
	printer.Policy.Media = smallFailureMedia
	printer.Policy.MediaSupported = []string{smallFailureMedia}
	printer.Policy.MediaDefault = smallFailureMedia
	svc.cfg.Printers["office"] = printer
	return svc
}
