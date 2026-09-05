package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

// CapabilityDump is the stable JSON envelope emitted by
// -dump-printer-capabilities. Attributes are deliberately kept as a list so
// repeated/unknown IPP attributes and their wire tags remain visible.
type CapabilityDump struct {
	Printers []PrinterCapabilityDump `json:"printers"`
}

type PrinterCapabilityDump struct {
	Queue       string          `json:"queue"`
	UpstreamURI string          `json:"upstream_uri"`
	DisplayName string          `json:"display_name"`
	Location    string          `json:"location"`
	Optional    bool            `json:"optional"`
	StatusCode  int             `json:"status_code,omitempty"`
	Status      string          `json:"status,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attributes  []DumpAttribute `json:"attributes,omitempty"`
}

type DumpAttribute struct {
	Name   string      `json:"name"`
	Values []DumpValue `json:"values"`
}

type DumpValue struct {
	Tag   string `json:"tag"`
	Value any    `json:"value"`
}

// CapabilityDumpError indicates that the JSON report was emitted but one or
// more configured printer probes failed. Callers can use it to return a
// non-zero process status without losing the partial report.
type CapabilityDumpError struct {
	Queues []string
}

func (e *CapabilityDumpError) Error() string {
	return fmt.Sprintf("capability probe failed for printer queues: %s", strings.Join(e.Queues, ", "))
}

// DumpPrinterCapabilities probes every configured printer, including optional
// printers, and writes one deterministic JSON report. It intentionally uses a
// client directly so dump mode does not open SQLite or start the HTTP server.
func DumpPrinterCapabilities(ctx context.Context, cfg *config.Config, output io.Writer) error {
	if cfg == nil {
		return errors.New("configuration is nil")
	}
	if output == nil {
		return errors.New("capability dump output is nil")
	}
	// Keep diagnostic logs away from the JSON stream, and at the same time
	// avoid logging upstream URIs (which may contain credentials).
	client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	queues := make([]string, 0, len(cfg.Printers))
	for queue := range cfg.Printers {
		queues = append(queues, queue)
	}
	sort.Strings(queues)

	report := CapabilityDump{Printers: make([]PrinterCapabilityDump, len(queues))}
	failedByIndex := make([]bool, len(queues))
	semaphore := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for index, queue := range queues {
		index, queue := index, queue
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			printer := cfg.Printers[queue]
			item := PrinterCapabilityDump{
				Queue:       queue,
				UpstreamURI: redactDumpString(printer.UpstreamURI),
				DisplayName: printer.DisplayName,
				Location:    printer.Location,
				Optional:    printer.Optional,
			}
			resp, err := client.GetPrinterAttributes(ctx, printer.UpstreamURI)
			if err != nil {
				item.Error = safeProbeError(err, printer.UpstreamURI)
				failedByIndex[index] = true
			} else if resp == nil {
				item.Error = "upstream returned an empty IPP response"
				failedByIndex[index] = true
			} else {
				item.StatusCode = int(resp.Code)
				item.Status = goipp.Status(resp.Code).String()
				item.Attributes = dumpAttributes(resp.Printer)
				if goipp.Status(resp.Code) >= goipp.StatusErrorBadRequest {
					item.Error = safeProbeError(errors.New(statusMessage(resp)), printer.UpstreamURI)
					if item.Error == "" {
						item.Error = item.Status
					}
					failedByIndex[index] = true
				}
			}
			report.Printers[index] = item
		}()
	}
	wg.Wait()
	failed := make([]string, 0)
	for index, isFailed := range failedByIndex {
		if isFailed {
			failed = append(failed, queues[index])
		}
	}

	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode capability dump: %w", err)
	}
	if len(failed) > 0 {
		return &CapabilityDumpError{Queues: failed}
	}
	return nil
}

func dumpAttributes(attrs goipp.Attributes) []DumpAttribute {
	if len(attrs) == 0 {
		return nil
	}
	// Sorting changes only the outer slice. Avoid DeepCopy here because legal
	// out-of-band values carry a nil Value that goipp cannot deep-copy.
	ordered := attrs.Clone()
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := strings.ToLower(ordered[i].Name), strings.ToLower(ordered[j].Name)
		if left == right {
			return ordered[i].Name < ordered[j].Name
		}
		return left < right
	})
	out := make([]DumpAttribute, 0, len(ordered))
	for _, attr := range ordered {
		values := make([]DumpValue, 0, len(attr.Values))
		for _, value := range attr.Values {
			dumped := any(nil)
			if sensitiveDumpAttribute(attr.Name) {
				dumped = "[redacted]"
			} else {
				dumped = dumpValue(value.V)
			}
			values = append(values, DumpValue{Tag: value.T.String(), Value: dumped})
		}
		out = append(out, DumpAttribute{
			Name:   attr.Name,
			Values: values,
		})
	}
	return out
}

func dumpValue(value goipp.Value) any {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case goipp.String:
		return redactDumpString(string(typed))
	case goipp.Integer:
		return int(typed)
	case goipp.Boolean:
		return bool(typed)
	case goipp.Range:
		return map[string]int{"lower": typed.Lower, "upper": typed.Upper}
	case goipp.Resolution:
		return map[string]any{"xres": typed.Xres, "yres": typed.Yres, "units": typed.Units.String()}
	case goipp.Collection:
		return dumpAttributes(goipp.Attributes(typed))
	case goipp.Binary:
		// encoding/json encodes []byte as base64, preserving the typed value
		// without accidentally treating it as document payload text.
		return []byte(typed)
	default:
		return redactDumpString(fmt.Sprint(value))
	}
}

func sensitiveDumpAttribute(name string) bool {
	name = strings.ToLower(name)
	for _, marker := range []string{"password", "credential", "secret", "auth-info", "access-token", "private-key"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func redactDumpString(value string) string {
	parsed, err := url.Parse(value)
	if err == nil && parsed.User != nil {
		parsed.User = url.User("redacted")
		return parsed.String()
	}
	return value
}

func safeProbeError(err error, upstreamURI string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	redacted := redactDumpString(upstreamURI)
	message = strings.ReplaceAll(message, upstreamURI, redacted)
	if converted, convertErr := iattr.HTTPURLFromIPP(upstreamURI); convertErr == nil {
		message = strings.ReplaceAll(message, converted, redactDumpString(converted))
	}
	return message
}
