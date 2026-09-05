package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

type UpstreamClient struct {
	HTTP             *http.Client
	MaxResponseBytes int64
	ProbeTimeout     time.Duration
	logger           *slog.Logger
}

func NewUpstreamClient(logger *slog.Logger) *UpstreamClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &UpstreamClient{
		HTTP: &http.Client{
			Timeout: 10 * time.Minute,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		MaxResponseBytes: 32 << 20,
		ProbeTimeout:     30 * time.Second,
		logger:           logger,
	}
}

func (c *UpstreamClient) Do(ctx context.Context, upstreamURI string, msg *goipp.Message, payload io.Reader) (*goipp.Message, error) {
	start := time.Now()
	op := goipp.Op(msg.Code)
	safeUpstreamURI := redactDumpString(upstreamURI)
	httpURL, err := iattr.HTTPURLFromIPP(upstreamURI)
	if err != nil {
		c.logger.Debug("upstream uri conversion failed",
			"upstream_uri", safeUpstreamURI,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"error", err,
		)
		return nil, err
	}
	safeHTTPURL := redactDumpString(httpURL)
	envelope, err := msg.EncodeBytes()
	if err != nil {
		c.logger.Debug("upstream ipp encode failed",
			"upstream_uri", safeUpstreamURI,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"error", err,
		)
		return nil, err
	}
	c.logger.Debug("upstream ipp request attributes",
		"upstream_uri", safeUpstreamURI,
		"http_url", safeHTTPURL,
		"ipp_version", msg.Version.String(),
		"operation", op.String(),
		"ipp_request_id", msg.RequestID,
		"envelope_bytes", len(envelope),
		"has_payload", payload != nil,
		"groups", logMessageGroups(msg),
	)
	var body io.Reader = bytes.NewReader(envelope)
	if payload != nil {
		body = io.MultiReader(bytes.NewReader(envelope), payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpURL, body)
	if err != nil {
		c.logger.Debug("upstream http request creation failed",
			"upstream_uri", safeUpstreamURI,
			"http_url", safeHTTPURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"error", err,
		)
		return nil, err
	}
	req.Header.Set("content-type", goipp.ContentType)
	req.Header.Set("accept", goipp.ContentType)
	c.logger.Debug("upstream ipp request started",
		"upstream_uri", safeUpstreamURI,
		"http_url", safeHTTPURL,
		"operation", op.String(),
		"ipp_request_id", msg.RequestID,
		"envelope_bytes", len(envelope),
		"has_payload", payload != nil,
	)

	resp, err := c.HTTP.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		c.logger.Debug("upstream ipp request failed",
			"upstream_uri", safeUpstreamURI,
			"http_url", safeHTTPURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"duration_ms", time.Since(start).Milliseconds(),
			"error", err,
		)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.logger.Debug("upstream ipp http status rejected",
			"upstream_uri", safeUpstreamURI,
			"http_url", safeHTTPURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"duration_ms", time.Since(start).Milliseconds(),
			"http_status", resp.Status,
		)
		return nil, errors.New(resp.Status)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("content-type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, goipp.ContentType) {
		return nil, fmt.Errorf("upstream returned content-type %q, expected %s", resp.Header.Get("content-type"), goipp.ContentType)
	}
	out := &goipp.Message{}
	responseBody := io.Reader(resp.Body)
	var bounded *maxBytesReader
	if c.MaxResponseBytes > 0 {
		bounded = &maxBytesReader{Reader: resp.Body, Max: c.MaxResponseBytes}
		responseBody = bounded
	}
	if err := out.Decode(responseBody); err != nil {
		c.logger.Debug("upstream ipp response decode failed",
			"upstream_uri", safeUpstreamURI,
			"http_url", safeHTTPURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"duration_ms", time.Since(start).Milliseconds(),
			"http_status", resp.Status,
			"error", err,
		)
		return nil, err
	}
	if bounded != nil {
		if _, err := io.Copy(io.Discard, bounded); err != nil {
			if errors.Is(err, errReadLimitExceeded) {
				return nil, fmt.Errorf("upstream IPP response exceeds %d bytes: %w", c.MaxResponseBytes, err)
			}
			return nil, fmt.Errorf("read upstream IPP response: %w", err)
		}
	}
	if out.RequestID != msg.RequestID {
		return nil, fmt.Errorf("upstream response request-id %d does not match request-id %d", out.RequestID, msg.RequestID)
	}
	if !supportedIPPVersion(out.Version) {
		return nil, fmt.Errorf("upstream response uses unsupported IPP version %s", out.Version)
	}
	if ippSuccess(out) && ippVersionGreater(out.Version, msg.Version) {
		return nil, fmt.Errorf("upstream response IPP version %s exceeds requested version %s", out.Version, msg.Version)
	}
	if err := validateUpstreamResponseGroups(out.Groups); err != nil {
		return nil, err
	}
	if len(out.Operation) < 2 || !singleValueAttr(out.Operation[0], "attributes-charset", goipp.TagCharset) ||
		!singleValueAttr(out.Operation[1], "attributes-natural-language", goipp.TagLanguage) {
		return nil, errors.New("upstream response is missing required leading operation attributes")
	}
	c.logger.Debug("upstream ipp response attributes",
		"upstream_uri", safeUpstreamURI,
		"http_url", safeHTTPURL,
		"ipp_version", out.Version.String(),
		"operation", op.String(),
		"ipp_request_id", out.RequestID,
		"duration_ms", time.Since(start).Milliseconds(),
		"http_status", resp.Status,
		"ipp_status", goipp.Status(out.Code).String(),
		"status_message", statusMessage(out),
		"groups", logMessageGroups(out),
	)
	c.logger.Debug("upstream ipp request completed",
		"upstream_uri", safeUpstreamURI,
		"http_url", safeHTTPURL,
		"operation", op.String(),
		"ipp_request_id", msg.RequestID,
		"duration_ms", time.Since(start).Milliseconds(),
		"http_status", resp.Status,
		"ipp_status", goipp.Status(out.Code).String(),
		"status_message", statusMessage(out),
		"operation_attr_count", len(out.Operation),
		"printer_attr_count", len(out.Printer),
		"job_attr_count", len(out.Job),
	)
	return out, nil
}

func ippVersionGreater(left, right goipp.Version) bool {
	if left.Major() != right.Major() {
		return left.Major() > right.Major()
	}
	return left.Minor() > right.Minor()
}

func validateUpstreamResponseGroups(groups goipp.Groups) error {
	if len(groups) == 0 || groups[0].Tag != goipp.TagOperationGroup {
		return errors.New("upstream response must begin with exactly one operation attributes group")
	}
	seenOperation := false
	seenUnsupported := false
	seenObject := false
	for index, group := range groups {
		switch group.Tag {
		case goipp.TagOperationGroup:
			if seenOperation || index != 0 {
				return errors.New("upstream response contains duplicate or misplaced operation attributes group")
			}
			seenOperation = true
		case goipp.TagUnsupportedGroup:
			if seenUnsupported || seenObject || index > 1 {
				return errors.New("upstream response contains duplicate or misplaced unsupported attributes group")
			}
			seenUnsupported = true
		default:
			seenObject = true
		}
	}
	return nil
}

type logGroup struct {
	Group string    `json:"group"`
	Attrs []logAttr `json:"attrs"`
}

type logAttr struct {
	Name   string     `json:"name"`
	Values []logValue `json:"values"`
}

type logValue struct {
	Tag   string `json:"tag"`
	Value string `json:"value"`
}

func logMessageGroups(msg *goipp.Message) []logGroup {
	if msg.Groups != nil {
		groups := make([]logGroup, 0, len(msg.Groups))
		for _, group := range msg.Groups {
			groups = append(groups, logGroup{
				Group: group.Tag.String(),
				Attrs: logAttributes(group.Attrs),
			})
		}
		return groups
	}

	groups := make([]logGroup, 0, 4)
	appendGroup := func(name string, attrs goipp.Attributes) {
		if len(attrs) == 0 {
			return
		}
		groups = append(groups, logGroup{
			Group: name,
			Attrs: logAttributes(attrs),
		})
	}
	appendGroup("operation-attributes-tag", msg.Operation)
	appendGroup("job-attributes-tag", msg.Job)
	appendGroup("printer-attributes-tag", msg.Printer)
	appendGroup("unsupported-attributes-tag", msg.Unsupported)
	appendGroup("subscription-attributes-tag", msg.Subscription)
	appendGroup("event-notification-attributes-tag", msg.EventNotification)
	appendGroup("resource-attributes-tag", msg.Resource)
	appendGroup("document-attributes-tag", msg.Document)
	appendGroup("system-attributes-tag", msg.System)
	return groups
}

func logAttributes(attrs goipp.Attributes) []logAttr {
	out := make([]logAttr, 0, len(attrs))
	for _, attr := range attrs {
		values := logValues(attr.Values)
		if sensitiveDumpAttribute(attr.Name) {
			for index := range values {
				values[index].Value = "[redacted]"
			}
		}
		out = append(out, logAttr{
			Name:   attr.Name,
			Values: values,
		})
	}
	return out
}

func logValues(values goipp.Values) []logValue {
	out := make([]logValue, 0, len(values))
	for _, value := range values {
		display := "<nil>"
		if value.V != nil {
			display = redactDumpString(value.V.String())
		}
		out = append(out, logValue{
			Tag:   value.T.String(),
			Value: display,
		})
	}
	return out
}

func (c *UpstreamClient) GetPrinterAttributes(ctx context.Context, upstreamURI string) (*goipp.Message, error) {
	if c.ProbeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.ProbeTimeout)
		defer cancel()
	}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation = iattr.BasicOperationAttrs(upstreamURI)
	req.Operation = append(req.Operation,
		goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("all")),
	)
	return c.Do(ctx, upstreamURI, req, nil)
}
