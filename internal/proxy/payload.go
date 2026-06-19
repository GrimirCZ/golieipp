package proxy

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
)

var pdfPageToken = regexp.MustCompile(`/Type\s*/Page\b`)

type PayloadMetadata struct {
	Bytes                int64
	PageCount            *int
	Copies               int
	EstimatedImpressions *int
}

type payloadRecorder struct {
	file *os.File
	body io.Reader
}

func newPayloadRecorder(payload io.Reader) (*payloadRecorder, error) {
	file, err := os.CreateTemp("", "golieipp-payload-*")
	if err != nil {
		return nil, err
	}
	return &payloadRecorder{
		file: file,
		body: io.TeeReader(payload, file),
	}, nil
}

func (r *payloadRecorder) Reader() io.Reader {
	return r.body
}

func (r *payloadRecorder) Finish(documentFormat string, copies int) (PayloadMetadata, error) {
	if copies < 1 {
		copies = 1
	}
	meta := PayloadMetadata{Copies: copies}
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

	if meta.Bytes > 0 && (isPDF(documentFormat) || fileLooksLikePDF(r.file.Name())) {
		pages, err := countPDFPagesBestEffort(r.file.Name())
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

func isPDF(documentFormat string) bool {
	documentFormat = strings.ToLower(strings.TrimSpace(documentFormat))
	return documentFormat == "application/pdf"
}

func fileLooksLikePDF(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	header := make([]byte, 1024)
	n, err := file.Read(header)
	if err != nil && err != io.EOF {
		return false
	}
	return bytes.Contains(header[:n], []byte("%PDF-"))
}

func countPDFPagesBestEffort(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	tail := ""
	count := 0
	buf := make([]byte, 64*1024)
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
