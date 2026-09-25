package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/grimir/golieipp/internal/stats"
)

var pdfPageToken = regexp.MustCompile(`/Type\s*/Page\b`)

type PayloadMetadata struct {
	Bytes                int64
	PageCount            *int
	Copies               int
	EstimatedImpressions *int
}

type payloadRecorder struct {
	file     *os.File
	body     io.Reader
	writer   *trackedFileWriter
	resource *stats.Resource
	release  sync.Once
	span     *stats.Span
}

// stagedPayload is a complete request document captured before dispatch. It
// is used only when the client did not provide Content-Length, because a
// streaming upstream request could otherwise be accepted before the proxy
// discovers a size or read error at the end of the client body.
type stagedPayload struct {
	file     *os.File
	path     string
	writer   *trackedFileWriter
	resource *stats.Resource
	span     *stats.Span
}

func stageUnknownPayload(ctx context.Context, source io.Reader, closeBody io.Closer, max int64) (*stagedPayload, error) {
	stageCtx, stageSpan := stats.BeginChild(ctx, stats.Action{Kind: "staging", Operation: "stage unknown-length payload"})
	_ = stageCtx
	stageOutcome := "error"
	defer func() {
		if stageSpan == nil {
			return
		}
		if ctx != nil && ctx.Err() != nil {
			stageOutcome = "canceled"
		}
		stageSpan.Finish(stageOutcome)
	}()
	if source == nil || closeBody == nil {
		return nil, errors.New("missing request body")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "golieipp-staged-payload-*")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	var fileResource *stats.Resource
	if stageSpan != nil {
		fileResource = stageSpan.AcquireTemp("staged-payload", 0)
	}
	tracked := &trackedFileWriter{file: file, resource: fileResource}
	staged := &stagedPayload{file: file, path: path, writer: tracked, resource: fileResource, span: stageSpan}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = staged.Close()
			_ = os.Remove(path)
		}
	}()

	// Closing the original HTTP request body is the portable way to interrupt a
	// blocked network read when the request context is canceled. The source may
	// be a buffered reader whose initial byte has already been peeked; the
	// callback must therefore close closeBody rather than source.
	stopClose := context.AfterFunc(ctx, func() { _ = closeBody.Close() })
	defer stopClose()

	limited := &maxBytesReader{Reader: source, Max: max}
	buffer := make([]byte, 64*1024)
	bufferResource := (*stats.Resource)(nil)
	if stageSpan != nil {
		bufferResource = stageSpan.AcquireBuffer("staged-payload-read", int64(cap(buffer)))
		defer bufferResource.Release()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := limited.Read(buffer)
		if n > 0 {
			if _, err := tracked.Write(buffer[:n]); err != nil {
				return nil, fmt.Errorf("stage request document: %w", err)
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ctx.Err()
		}
		if readErr == io.EOF {
			break
		}
		return nil, readErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind staged request document: %w", err)
	}
	removeOnError = false
	stageOutcome = "success"
	return staged, nil
}

func (p *stagedPayload) Reader() io.Reader {
	if p == nil {
		return nil
	}
	return p.file
}

func (p *stagedPayload) Close() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.file.Close(), os.Remove(p.path), releaseResource(p.resource))
}

func newPayloadRecorder(payload io.Reader) (*payloadRecorder, error) {
	return newPayloadRecorderWithContext(context.Background(), payload)
}

// newPayloadRecorderContext is the context-aware form used by instrumented
// request paths. The original helper remains for compatibility with tests and
// callers that do not collect statistics.
func newPayloadRecorderContext(ctx context.Context, payload io.Reader) (*payloadRecorder, error) {
	return newPayloadRecorderWithContext(ctx, payload)
}

func newPayloadRecorderWithContext(ctx context.Context, payload io.Reader) (*payloadRecorder, error) {
	_, span := stats.BeginChild(ctx, stats.Action{Kind: "payload-recording", Operation: "record payload metadata"})
	file, err := os.CreateTemp("", "golieipp-payload-*")
	if err != nil {
		span.Finish("error")
		return nil, err
	}
	var resource *stats.Resource
	if span != nil {
		resource = span.AcquireTemp("payload-recording", 0)
	}
	writer := &trackedFileWriter{file: file, resource: resource}
	return &payloadRecorder{file: file, body: io.TeeReader(payload, writer), writer: writer, resource: resource, span: span}, nil
}

func (r *payloadRecorder) Reader() io.Reader {
	return r.body
}

func (r *payloadRecorder) Finish(documentFormat string, copies int) (meta PayloadMetadata, retErr error) {
	if r == nil {
		return PayloadMetadata{}, errors.New("nil payload recorder")
	}
	defer r.releaseResource()
	defer func() { r.span.Finish(outcomeForError(retErr)) }()
	if copies < 1 {
		copies = 1
	}
	meta = PayloadMetadata{Copies: copies}
	if err := r.file.Close(); err != nil {
		_ = os.Remove(r.file.Name())
		return meta, err
	}
	defer os.Remove(r.file.Name())

	stat, err := os.Stat(r.file.Name())
	if err != nil {
		return meta, err
	}
	meta.Bytes = stat.Size()

	if meta.Bytes > 0 && (isPDF(documentFormat) || fileLooksLikePDF(r.file.Name(), r.span)) {
		pages, err := countPDFPagesBestEffort(r.file.Name(), r.span)
		if err != nil {
			return meta, err
		}
		if pages > 0 {
			meta.PageCount = &pages
			impressions := pages * copies
			meta.EstimatedImpressions = &impressions
		}
	}
	return meta, nil
}

// Close releases a recorder after a failed or canceled upstream request.
// Finish also releases the tracked temporary resource, so callers may safely
// defer Close and call Finish on the success path.
func (r *payloadRecorder) Close() error {
	if r == nil {
		return nil
	}
	return errors.Join(r.file.Close(), os.Remove(r.file.Name()), r.releaseResource())
}

func (r *payloadRecorder) releaseResource() error {
	if r == nil {
		return nil
	}
	r.release.Do(func() {
		if r.resource != nil {
			r.resource.Release()
		}
	})
	return nil
}

func releaseResource(resource *stats.Resource) error {
	if resource != nil {
		resource.Release()
	}
	return nil
}

type trackedFileWriter struct {
	file     *os.File
	resource *stats.Resource
	logical  int64
}

func (w *trackedFileWriter) Write(p []byte) (int, error) {
	if w == nil || w.file == nil {
		return 0, errors.New("nil tracked file")
	}
	n, err := w.file.Write(p)
	if n > 0 {
		w.logical += int64(n)
		if w.resource != nil {
			w.resource.Written(int64(n))
			w.resource.SetSize(w.logical)
		}
	}
	return n, err
}

func isPDF(documentFormat string) bool {
	documentFormat = strings.ToLower(strings.TrimSpace(documentFormat))
	return documentFormat == "application/pdf"
}

func fileLooksLikePDF(path string, spans ...*stats.Span) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	header := make([]byte, 1024)
	if len(spans) > 0 {
		defer spans[0].AcquireBuffer("payload-format-sniff", int64(cap(header))).Release()
	}
	n, err := file.Read(header)
	if err != nil && err != io.EOF {
		return false
	}
	return bytes.Contains(header[:n], []byte("%PDF-"))
}

func countPDFPagesBestEffort(path string, tracked ...*stats.Span) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	tail := ""
	count := 0
	buf := make([]byte, 64*1024)
	var bufferResource *stats.Resource
	if len(tracked) > 0 && tracked[0] != nil {
		bufferResource = tracked[0].AcquireBuffer("payload-pdf-scan", int64(cap(buf)+reader.Size()))
		defer bufferResource.Release()
	}
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			chunk := tail + string(buf[:n])
			tailLen := len(tail)
			for _, match := range pdfPageToken.FindAllStringIndex(chunk, -1) {
				if match[1] > tailLen {
					count++
				}
			}
			if len(chunk) > 128 {
				tail = chunk[len(chunk)-128:]
			} else {
				tail = chunk
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}
	return count, nil
}
