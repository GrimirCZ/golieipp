package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestUpstreamValidatesHTTPAndNegotiatedIPPResponse(t *testing.T) {
	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 91)
	request.Operation = responseOperationAttrs("")

	tests := []struct {
		name        string
		status      int
		contentType string
		responseID  uint32
		version     goipp.Version
		valid       bool
	}{
		{name: "valid", status: http.StatusOK, contentType: goipp.ContentType, responseID: 91, version: goipp.DefaultVersion, valid: true},
		{name: "closest supported 1.1", status: http.StatusOK, contentType: goipp.ContentType, responseID: 91, version: goipp.MakeVersion(1, 1), valid: true},
		{name: "non 200", status: http.StatusCreated, contentType: goipp.ContentType, responseID: 91, version: goipp.DefaultVersion},
		{name: "wrong media type", status: http.StatusOK, contentType: "text/plain", responseID: 91, version: goipp.DefaultVersion},
		{name: "wrong request id", status: http.StatusOK, contentType: goipp.ContentType, responseID: 92, version: goipp.DefaultVersion},
		{name: "unsupported version", status: http.StatusOK, contentType: goipp.ContentType, responseID: 91, version: goipp.MakeVersion(3, 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("content-type", test.contentType)
				w.WriteHeader(test.status)
				response := goipp.NewResponse(test.version, goipp.StatusOk, test.responseID)
				response.Operation = responseOperationAttrs("")
				_ = response.Encode(w)
			}))
			defer server.Close()

			client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
			_, err := client.Do(context.Background(), strings.Replace(server.URL, "http://", "ipp://", 1), request, nil)
			if test.valid && err != nil {
				t.Fatalf("valid response rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid upstream response accepted")
			}
		})
	}
}

func TestUpstreamRejectsSuccessfulResponseVersionAboveRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", goipp.ContentType)
		response := goipp.NewResponse(goipp.MakeVersion(2, 0), goipp.StatusOk, 95)
		response.Operation = responseOperationAttrs("")
		if err := response.Encode(w); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	request := goipp.NewRequest(goipp.MakeVersion(1, 1), goipp.OpGetPrinterAttributes, 95)
	request.Operation = responseOperationAttrs("")
	client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := client.Do(context.Background(), strings.Replace(server.URL, "http://", "ipp://", 1), request, nil); err == nil {
		t.Fatal("successful upstream response upgraded the requested IPP version")
	}
}

func TestUpstreamAllowsVersionNotSupportedResponseAboveRequest(t *testing.T) {
	response := goipp.NewResponse(goipp.MakeVersion(2, 0), goipp.StatusErrorVersionNotSupported, 96)
	response.Operation = responseOperationAttrs("")
	envelope, err := response.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}

	client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.HTTP = &http.Client{Transport: upstreamRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{goipp.ContentType}},
			Body:       io.NopCloser(bytes.NewReader(envelope)),
			Request:    req,
		}, nil
	})}

	request := goipp.NewRequest(goipp.MakeVersion(1, 1), goipp.OpGetPrinterAttributes, 96)
	request.Operation = responseOperationAttrs("")
	out, err := client.Do(context.Background(), "ipp://printer.invalid/ipp/print", request, nil)
	if err != nil {
		t.Fatalf("legal version-negotiation error response rejected: %v", err)
	}
	if out.Version != goipp.MakeVersion(2, 0) {
		t.Fatalf("response version = %s, want 2.0", out.Version)
	}
	if goipp.Status(out.Code) != goipp.StatusErrorVersionNotSupported {
		t.Fatalf("response status = %s, want %s", goipp.Status(out.Code), goipp.StatusErrorVersionNotSupported)
	}
}

type upstreamRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f upstreamRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUpstreamRejectsRedirect(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 92)
	request.Operation = responseOperationAttrs("")
	client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := client.Do(context.Background(), strings.Replace(redirect.URL, "http://", "ipp://", 1), request, nil); err == nil {
		t.Fatal("redirect was followed")
	}
	if destinationCalls != 0 {
		t.Fatalf("redirect destination called %d times", destinationCalls)
	}
}

func TestUpstreamEnforcesResponseByteLimitAfterDecodedEnvelope(t *testing.T) {
	response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 93)
	response.Operation = responseOperationAttrs("")
	envelope, err := response.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", goipp.ContentType)
		_, _ = w.Write(envelope)
		_, _ = io.CopyN(w, bytes.NewReader(bytes.Repeat([]byte{'x'}, 64)), 64)
	}))
	defer server.Close()

	request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 93)
	request.Operation = responseOperationAttrs("")
	client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.MaxResponseBytes = int64(len(envelope) + 8)
	if _, err := client.Do(context.Background(), strings.Replace(server.URL, "http://", "ipp://", 1), request, nil); err == nil {
		t.Fatal("oversized upstream response was accepted")
	}
}

func TestUpstreamRejectsMalformedResponseGroups(t *testing.T) {
	tests := []struct {
		name   string
		groups goipp.Groups
	}{
		{
			name: "unsupported after job",
			groups: goipp.Groups{
				{Tag: goipp.TagOperationGroup, Attrs: responseOperationAttrs("")},
				{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{iattr.Integer("job-id", 7)}},
				{Tag: goipp.TagUnsupportedGroup, Attrs: goipp.Attributes{iattr.Keyword("media", "bad")}},
			},
		},
		{
			name: "duplicate operation group",
			groups: goipp.Groups{
				{Tag: goipp.TagOperationGroup, Attrs: responseOperationAttrs("")},
				{Tag: goipp.TagOperationGroup, Attrs: goipp.Attributes{iattr.Text("status-message", "duplicate")}},
			},
		},
		{
			name: "duplicate unsupported group",
			groups: goipp.Groups{
				{Tag: goipp.TagOperationGroup, Attrs: responseOperationAttrs("")},
				{Tag: goipp.TagUnsupportedGroup, Attrs: goipp.Attributes{iattr.Keyword("media", "bad")}},
				{Tag: goipp.TagUnsupportedGroup, Attrs: goipp.Attributes{iattr.Keyword("sides", "bad")}},
			},
		},
		{
			name: "missing operation group",
			groups: goipp.Groups{
				{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{iattr.Integer("job-id", 7)}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("content-type", goipp.ContentType)
				response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 94)
				response.Groups = test.groups
				if err := response.Encode(w); err != nil {
					t.Errorf("encode malformed response: %v", err)
				}
			}))
			defer server.Close()

			request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 94)
			request.Operation = responseOperationAttrs("")
			client := NewUpstreamClient(slog.New(slog.NewTextHandler(io.Discard, nil)))
			if _, err := client.Do(context.Background(), strings.Replace(server.URL, "http://", "ipp://", 1), request, nil); err == nil {
				t.Fatal("malformed response groups were accepted")
			}
		})
	}
}
