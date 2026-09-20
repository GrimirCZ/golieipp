package proxy

import (
	"io"
	"log/slog"
	"testing"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestPayloadAdmissionUsesCommittedEffectiveFormats(t *testing.T) {
	printer := config.PrinterConfig{
		UpstreamURI: "ipp://upstream.example/ipp/print",
		Policy: config.PolicyConfig{
			Media:          "iso_a4_210x297mm",
			MediaType:      "stationery",
			PrintColorMode: "monochrome",
		},
	}
	cfg := &config.Config{
		Listen: config.ListenConfig{PublicBaseURL: "ipp://proxy.example/printers"},
		Printers: map[string]config.PrinterConfig{
			"office": printer,
		},
	}
	svc, err := NewService(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	upstream := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttr("document-format-supported", goipp.TagMimeType,
			goipp.String("application/pdf"), goipp.String("application/octet-stream")),
	}
	model := NewCapabilityModel(upstream, "office", svc.proxyPrinterURI("office"), printer, CapabilityModelOptions{
		Operations:        []goipp.Op{goipp.OpGetPrinterAttributes, goipp.OpPrintJob},
		PracticalProfiles: true,
	}, defaultPrinterIdentityMetadata(svc.proxyPrinterURI("office")))
	svc.mu.Lock()
	svc.capabilities["office"] = upstream
	svc.capabilityModels["office"] = model
	svc.mu.Unlock()

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 1)
	request.Operation = append(iattr.BasicOperationAttrs(svc.proxyPrinterURI("office")),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")))
	if protocolErr := svc.validatePayloadDocumentFormat("office", request); protocolErr != nil {
		t.Fatalf("admitted PDF was rejected: %v", protocolErr)
	}
	request.Operation = iattr.SetAttr(request.Operation,
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/octet-stream")))
	if protocolErr := svc.validatePayloadDocumentFormat("office", request); protocolErr == nil || protocolErr.Status != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("octet-stream was admitted: %#v", protocolErr)
	}

	noFormats := NewCapabilityModel(goipp.Attributes{iattr.Keyword("media-supported", "iso_a4_210x297mm")}, "office", svc.proxyPrinterURI("office"), printer, CapabilityModelOptions{
		Operations:        []goipp.Op{goipp.OpGetPrinterAttributes, goipp.OpPrintJob},
		PracticalProfiles: true,
	}, defaultPrinterIdentityMetadata(svc.proxyPrinterURI("office")))
	svc.mu.Lock()
	svc.capabilityModels["office"] = noFormats
	svc.mu.Unlock()
	if protocolErr := svc.validatePayloadDocumentFormat("office", request); protocolErr == nil || protocolErr.Status != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("payload was admitted without a successful format capability: %#v", protocolErr)
	}
}
