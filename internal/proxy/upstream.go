package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

type UpstreamClient struct {
	HTTP   *http.Client
	logger *slog.Logger
}

func NewUpstreamClient(logger *slog.Logger) *UpstreamClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &UpstreamClient{
		HTTP:   &http.Client{Timeout: 10 * time.Minute},
		logger: logger,
	}
}

func (c *UpstreamClient) Do(ctx context.Context, upstreamURI string, msg *goipp.Message, payload io.Reader) (*goipp.Message, error) {
	start := time.Now()
	op := goipp.Op(msg.Code)
	httpURL, err := iattr.HTTPURLFromIPP(upstreamURI)
	if err != nil {
		c.logger.Debug("upstream uri conversion failed",
			"upstream_uri", upstreamURI,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"error", err,
		)
		return nil, err
	}
	envelope, err := msg.EncodeBytes()
	if err != nil {
		c.logger.Debug("upstream ipp encode failed",
			"upstream_uri", upstreamURI,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"error", err,
		)
		return nil, err
	}
	c.logger.Debug("upstream ipp request attributes",
		"upstream_uri", upstreamURI,
		"http_url", httpURL,
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
			"upstream_uri", upstreamURI,
			"http_url", httpURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"error", err,
		)
		return nil, err
	}
	req.Header.Set("content-type", goipp.ContentType)
	req.Header.Set("accept", goipp.ContentType)
	c.logger.Debug("upstream ipp request started",
		"upstream_uri", upstreamURI,
		"http_url", httpURL,
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
			"upstream_uri", upstreamURI,
			"http_url", httpURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"duration_ms", time.Since(start).Milliseconds(),
			"error", err,
		)
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		c.logger.Debug("upstream ipp http status rejected",
			"upstream_uri", upstreamURI,
			"http_url", httpURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"duration_ms", time.Since(start).Milliseconds(),
			"http_status", resp.Status,
		)
		return nil, errors.New(resp.Status)
	}
	out := &goipp.Message{}
	if err := out.Decode(resp.Body); err != nil {
		c.logger.Debug("upstream ipp response decode failed",
			"upstream_uri", upstreamURI,
			"http_url", httpURL,
			"operation", op.String(),
			"ipp_request_id", msg.RequestID,
			"duration_ms", time.Since(start).Milliseconds(),
			"http_status", resp.Status,
			"error", err,
		)
		return nil, err
	}
	c.logger.Debug("upstream ipp response attributes",
		"upstream_uri", upstreamURI,
		"http_url", httpURL,
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
		"upstream_uri", upstreamURI,
		"http_url", httpURL,
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
		out = append(out, logAttr{
			Name:   attr.Name,
			Values: logValues(attr.Values),
		})
	}
	return out
}

func logValues(values goipp.Values) []logValue {
	out := make([]logValue, 0, len(values))
	for _, value := range values {
		display := "<nil>"
		if value.V != nil {
			display = value.V.String()
		}
		out = append(out, logValue{
			Tag:   value.T.String(),
			Value: display,
		})
	}
	return out
}

func (c *UpstreamClient) GetPrinterAttributes(ctx context.Context, upstreamURI string) (*goipp.Message, error) {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, uint32(time.Now().UnixNano()))
	req.Operation = iattr.BasicOperationAttrs(upstreamURI)
	req.Operation = append(req.Operation,
		goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("all")),
	)
	return c.Do(ctx, upstreamURI, req, nil)
}
