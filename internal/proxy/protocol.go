package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

var errReadLimitExceeded = errors.New("IPP request exceeds configured size limit")

type phaseLimitReader struct {
	Reader io.Reader
	Max    int64
	read   int64
}

func (r *phaseLimitReader) Read(p []byte) (int, error) {
	if r.Max > 0 && r.read >= r.Max {
		return 0, errReadLimitExceeded
	}
	if remaining := r.Max - r.read; r.Max > 0 && int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

type maxBytesReader struct {
	Reader io.Reader
	Max    int64
	read   int64
}

func (r *maxBytesReader) Read(p []byte) (int, error) {
	if r.Max > 0 && r.read >= r.Max {
		var probe [1]byte
		n, err := r.Reader.Read(probe[:])
		if n > 0 {
			return 0, errReadLimitExceeded
		}
		return 0, err
	}
	if remaining := r.Max - r.read; r.Max > 0 && int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

// ProtocolError is an IPP client error found before an operation is dispatched.
// Keeping this typed lets every handler produce the same standards-shaped
// response without leaking transport or storage errors into status-message.
type ProtocolError struct {
	Status      goipp.Status
	Message     string
	Unsupported goipp.Attributes
}

func (e *ProtocolError) Error() string { return e.Message }

// ValidateProtocolRequest validates invariants that are common to every IPP
// operation. Operation-specific business rules remain in the dispatcher.
func ValidateProtocolRequest(msg *goipp.Message, printerURI string, hasPayload bool) *ProtocolError {
	if msg == nil {
		return protocolError(goipp.StatusErrorBadRequest, "empty IPP request")
	}
	if err := validateRequestGroups(msg); err != nil {
		return err
	}
	op := goipp.Op(msg.Code)
	if err := validateRequiredOperationAttrs(msg.Operation, op); err != nil {
		return protocolError(goipp.StatusErrorBadRequest, err.Error())
	}
	charset, _ := iattr.FirstString(msg.Operation, "attributes-charset")
	if !strings.EqualFold(charset, "utf-8") {
		return &ProtocolError{Status: goipp.StatusErrorCharset, Message: "only utf-8 is supported", Unsupported: attrsNamed(msg.Operation, "attributes-charset")}
	}
	if err := validateNoDuplicateAttrs(msg); err != nil {
		return err
	}
	if err := validateTarget(msg.Operation, op, printerURI); err != nil {
		return err
	}
	// A media/media-col conflict is more specific than the syntax of either
	// value and must be reported as such even when one of them is malformed.
	if _, media := iattr.Attr(msg.Job, "media"); media {
		if _, mediaCol := iattr.Attr(msg.Job, "media-col"); mediaCol {
			return &ProtocolError{Status: goipp.StatusErrorConflicting, Message: "media and media-col cannot both be supplied", Unsupported: attrsNamed(msg.Job, "media", "media-col")}
		}
	}
	if err := validateKnownValues(msg); err != nil {
		return err
	}
	if attr, ok := iattr.Attr(msg.Job, "overrides"); ok {
		if err := validateOverrides(attr); err != nil {
			return err
		}
	}
	if op == goipp.OpPrintJob {
		if !hasPayload {
			return protocolError(goipp.StatusErrorBadRequest, "document data is required")
		}
	}
	if op == goipp.OpSendDocument {
		attr, ok := iattr.Attr(msg.Operation, "last-document")
		if !ok || len(attr.Values) != 1 || attr.Values[0].T != goipp.TagBoolean {
			return protocolError(goipp.StatusErrorBadRequest, "last-document boolean is required")
		}
		lastDocument, valueOK := attr.Values[0].V.(goipp.Boolean)
		if !valueOK {
			return protocolError(goipp.StatusErrorBadRequest, "last-document boolean is required")
		}
		if !hasPayload && !bool(lastDocument) {
			return protocolError(goipp.StatusErrorBadRequest, "document data is required unless last-document is true")
		}
	}
	return nil
}

func validateExplicitDocumentFormat(attrs goipp.Attributes) *ProtocolError {
	attr, ok := iattr.Attr(attrs, "document-format")
	if !ok || len(attr.Values) != 1 || attr.Values[0].T != goipp.TagMimeType {
		return &ProtocolError{Status: goipp.StatusErrorBadRequest, Message: "document-format is required for a payload-bearing operation", Unsupported: attrsNamed(attrs, "document-format")}
	}
	value, ok := attr.Values[0].V.(goipp.String)
	if !ok || strings.TrimSpace(string(value)) == "" {
		return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "document-format has invalid value syntax", Unsupported: goipp.Attributes{attr}}
	}
	return nil
}

func protocolError(status goipp.Status, message string) *ProtocolError {
	return &ProtocolError{Status: status, Message: message}
}

func validateRequestGroups(msg *goipp.Message) *ProtocolError {
	if msg.Groups == nil {
		return nil
	}
	if len(msg.Groups) == 0 || msg.Groups[0].Tag != goipp.TagOperationGroup {
		return protocolError(goipp.StatusErrorBadRequest, "operation attributes must be the first group")
	}
	operationGroups := 0
	jobGroups := 0
	for _, group := range msg.Groups {
		switch group.Tag {
		case goipp.TagOperationGroup:
			operationGroups++
			if operationGroups != 1 || jobGroups > 0 {
				return protocolError(goipp.StatusErrorBadRequest, "operation attribute group is duplicated or out of order")
			}
		case goipp.TagJobGroup:
			jobGroups++
			if jobGroups > 1 {
				return protocolError(goipp.StatusErrorBadRequest, "job attribute group is duplicated")
			}
		case goipp.TagDocumentGroup:
			return protocolError(goipp.StatusErrorBadRequest, "document attribute groups are not supported")
		default:
			return protocolError(goipp.StatusErrorBadRequest, fmt.Sprintf("attribute group %s is not valid in this request", group.Tag))
		}
	}
	if jobGroups > 0 {
		switch goipp.Op(msg.Code) {
		case goipp.OpPrintJob, goipp.OpValidateJob, goipp.OpCreateJob:
		default:
			return protocolError(goipp.StatusErrorBadRequest, "job attributes are not valid for this operation")
		}
	}
	return nil
}

func validateNoDuplicateAttrs(msg *goipp.Message) *ProtocolError {
	groups := []goipp.Attributes{msg.Operation, msg.Job, msg.Document, msg.Subscription}
	for _, attrs := range groups {
		seen := make(map[string]struct{}, len(attrs))
		for _, attr := range attrs {
			name := strings.ToLower(attr.Name)
			if _, exists := seen[name]; exists {
				return &ProtocolError{Status: goipp.StatusErrorBadRequest, Message: "duplicate attribute: " + attr.Name, Unsupported: goipp.Attributes{attr}}
			}
			seen[name] = struct{}{}
		}
	}
	return nil
}

func validateTarget(attrs goipp.Attributes, op goipp.Op, printerURI string) *ProtocolError {
	printer, hasPrinter := iattr.FirstString(attrs, "printer-uri")
	jobURI, hasJobURI := iattr.FirstString(attrs, "job-uri")
	jobID, hasJobID := iattr.FirstInt(attrs, "job-id")

	if printerOperationRequiresURI(op) {
		if len(attrs) < 3 || !singleValueAttr(attrs[2], "printer-uri", goipp.TagURI) {
			return protocolError(goipp.StatusErrorBadRequest, "printer-uri must be the third operation attribute")
		}
		if !hasPrinter || !sameTargetURI(printer, printerURI) {
			return protocolError(goipp.StatusErrorNotFound, "printer-uri does not identify this queue")
		}
		if hasJobURI || hasJobID {
			return protocolError(goipp.StatusErrorBadRequest, "printer operation contains a job target")
		}
		return nil
	}
	if !jobOperationRequiresTarget(op) {
		return nil
	}
	if hasJobURI {
		if len(attrs) < 3 || !singleValueAttr(attrs[2], "job-uri", goipp.TagURI) {
			return protocolError(goipp.StatusErrorBadRequest, "job-uri must be the third operation attribute")
		}
		if hasPrinter || hasJobID {
			return protocolError(goipp.StatusErrorBadRequest, "supply either job-uri or printer-uri with job-id")
		}
		if _, valid := jobIDForPrinterURI(jobURI, printerURI); !valid {
			return protocolError(goipp.StatusErrorNotFound, "job-uri does not identify a job in this queue")
		}
		return nil
	}
	if len(attrs) < 4 || !singleValueAttr(attrs[2], "printer-uri", goipp.TagURI) || !singleValueAttr(attrs[3], "job-id", goipp.TagInteger) {
		return protocolError(goipp.StatusErrorBadRequest, "printer-uri and job-id must be the third and fourth operation attributes")
	}
	if !hasPrinter || !sameTargetURI(printer, printerURI) || !hasJobID || jobID < 1 {
		return protocolError(goipp.StatusErrorNotFound, "printer-uri and positive job-id are required")
	}
	return nil
}

func sameTargetURI(left, right string) bool {
	l, lerr := url.Parse(left)
	r, rerr := url.Parse(right)
	if lerr != nil || rerr != nil || l.Scheme == "" || l.Host == "" || r.Scheme == "" || r.Host == "" {
		return false
	}
	if !strings.EqualFold(l.Scheme, r.Scheme) || l.RawQuery != "" || r.RawQuery != "" || l.Fragment != "" || r.Fragment != "" {
		return false
	}
	// DNS-SD supplies the hostname and port used by the client. The HTTP
	// router has already selected the queue from the request path, so the
	// authority is not part of the queue identity here. Keep the URI scheme
	// and resource path meaningful while allowing mDNS names, aliases, and
	// explicit/default ports to vary between discovery and the request.
	return strings.TrimRight(l.EscapedPath(), "/") == strings.TrimRight(r.EscapedPath(), "/")
}

func jobOperationRequiresTarget(op goipp.Op) bool {
	switch op {
	case goipp.OpSendDocument, goipp.OpGetJobAttributes, goipp.OpCancelJob, goipp.OpCloseJob:
		return true
	default:
		return false
	}
}

func validateKnownValues(msg *goipp.Message) *ProtocolError {
	type rule struct {
		attrs       goipp.Attributes
		name        string
		tag         goipp.Tag
		single      bool
		positiveInt bool
	}
	rules := []rule{
		{msg.Operation, "attributes-charset", goipp.TagCharset, true, false},
		{msg.Operation, "attributes-natural-language", goipp.TagLanguage, true, false},
		{msg.Operation, "printer-uri", goipp.TagURI, true, false},
		{msg.Operation, "job-uri", goipp.TagURI, true, false},
		{msg.Operation, "job-id", goipp.TagInteger, true, true},
		{msg.Operation, "last-document", goipp.TagBoolean, true, false},
		{msg.Operation, "document-format", goipp.TagMimeType, true, false},
		{msg.Operation, "ipp-attribute-fidelity", goipp.TagBoolean, true, false},
		{msg.Operation, "my-jobs", goipp.TagBoolean, true, false},
		{msg.Operation, "limit", goipp.TagInteger, true, true},
		{msg.Operation, "which-jobs", goipp.TagKeyword, true, false},
		{msg.Job, "copies", goipp.TagInteger, true, true},
		{msg.Job, "media", goipp.TagKeyword, true, false},
		{msg.Job, "media-col", goipp.TagBeginCollection, true, false},
	}
	for _, rule := range rules {
		attr, ok := iattr.Attr(rule.attrs, rule.name)
		if !ok {
			continue
		}
		if (rule.single && len(attr.Values) != 1) || len(attr.Values) == 0 {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: rule.name + " has invalid cardinality", Unsupported: goipp.Attributes{attr}}
		}
		for _, value := range attr.Values {
			if value.T != rule.tag {
				return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: rule.name + " has invalid value syntax", Unsupported: goipp.Attributes{attr}}
			}
			if rule.positiveInt {
				integer, ok := value.V.(goipp.Integer)
				if !ok || integer < 1 {
					return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: rule.name + " must be positive", Unsupported: goipp.Attributes{attr}}
				}
			}
		}
		if rule.name == "media-col" && !validMediaColSyntax(attr) {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "media-col has invalid collection members", Unsupported: goipp.Attributes{attr}}
		}
	}
	if attr, ok := iattr.Attr(msg.Operation, "requested-attributes"); ok {
		if len(attr.Values) == 0 {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "requested-attributes requires at least one keyword", Unsupported: goipp.Attributes{attr}}
		}
		for _, value := range attr.Values {
			if value.T != goipp.TagKeyword {
				return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "requested-attributes has invalid value syntax", Unsupported: goipp.Attributes{attr}}
			}
		}
	}
	if value, ok := iattr.FirstString(msg.Operation, "which-jobs"); ok {
		switch strings.ToLower(value) {
		case "completed", "not-completed", "aborted", "all", "canceled", "pending", "pending-held", "processing", "processing-stopped":
		default:
			attr, _ := iattr.Attr(msg.Operation, "which-jobs")
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "which-jobs has unsupported value", Unsupported: goipp.Attributes{attr}}
		}
	}
	return nil
}

func jobIDForPrinterURI(rawJobURI, printerURI string) (int, bool) {
	job, jobErr := url.Parse(rawJobURI)
	printer, printerErr := url.Parse(printerURI)
	if jobErr != nil || printerErr != nil || job.RawQuery != "" || job.Fragment != "" {
		return 0, false
	}
	if job.Scheme == "" || printer.Scheme == "" || job.Host == "" || printer.Host == "" || !strings.EqualFold(job.Scheme, printer.Scheme) {
		return 0, false
	}
	prefix := strings.TrimRight(printer.EscapedPath(), "/") + "/jobs/"
	if !strings.HasPrefix(job.EscapedPath(), prefix) {
		return 0, false
	}
	tail := strings.TrimPrefix(job.EscapedPath(), prefix)
	if tail == "" || strings.Contains(tail, "/") {
		return 0, false
	}
	id, err := strconv.Atoi(tail)
	return id, err == nil && id > 0
}

func validateOverrides(attr goipp.Attribute) *ProtocolError {
	if len(attr.Values) == 0 {
		return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "overrides requires at least one collection", Unsupported: goipp.Attributes{attr}}
	}
	for _, value := range attr.Values {
		collection, ok := value.V.(goipp.Collection)
		if value.T != goipp.TagBeginCollection || !ok {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "overrides values must be collections", Unsupported: goipp.Attributes{attr}}
		}
		attrs := goipp.Attributes(collection)
		pagesAttr, pages := iattr.Attr(attrs, "pages")
		documentsAttr, documents := iattr.Attr(attrs, "document-numbers")
		if pages == documents {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "each override requires exactly one pages or document-numbers selector", Unsupported: goipp.Attributes{attr}}
		}
		if len(attrs) < 2 {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "override has no job-template attributes", Unsupported: goipp.Attributes{attr}}
		}
		selector := pagesAttr
		if documents {
			selector = documentsAttr
		}
		if !validPositiveRanges(selector) {
			return &ProtocolError{Status: goipp.StatusErrorAttributesOrValues, Message: "override selector must contain positive ranges", Unsupported: goipp.Attributes{attr}}
		}
	}
	return nil
}

func validPositiveRanges(attr goipp.Attribute) bool {
	if len(attr.Values) == 0 {
		return false
	}
	for _, value := range attr.Values {
		rangeValue, ok := value.V.(goipp.Range)
		if value.T != goipp.TagRange || !ok || rangeValue.Lower < 1 || rangeValue.Upper < rangeValue.Lower {
			return false
		}
	}
	return true
}

func validMediaColSyntax(attr goipp.Attribute) bool {
	if len(attr.Values) != 1 {
		return false
	}
	if attr.Values[0].T != goipp.TagBeginCollection {
		return false
	}
	collection, ok := attr.Values[0].V.(goipp.Collection)
	if !ok {
		return false
	}

	state := mediaColSyntaxState{seen: make(map[string]struct{})}
	if !validMediaColMembers(goipp.Attributes(collection), &state, false, nil) {
		return false
	}
	return state.hasSelector
}

// mediaColSyntaxState is shared by every nested collection in a media-col
// value. The normalizer also walks these collections recursively, so member
// names must be unique across the whole tree rather than only in the first
// collection encountered.
type mediaColSyntaxState struct {
	seen        map[string]struct{}
	hasSelector bool
}

type mediaColDimensions struct {
	hasX bool
	hasY bool
}

func validMediaColMembers(attrs goipp.Attributes, state *mediaColSyntaxState, inMediaSize bool, dimensions *mediaColDimensions) bool {
	for _, member := range attrs {
		name := strings.ToLower(member.Name)
		if name == "" {
			return false
		}
		if _, duplicate := state.seen[name]; duplicate {
			return false
		}
		state.seen[name] = struct{}{}

		switch name {
		case "media-size-name", "media-key":
			if !validMediaColKeyword(member) {
				return false
			}
			state.hasSelector = true
		case "media-size":
			if inMediaSize || !validMediaSizeMember(member, state) {
				return false
			}
			state.hasSelector = true
		case "x-dimension":
			if !inMediaSize || dimensions == nil || !validMediaColPositiveInteger(member) {
				return false
			}
			dimensions.hasX = true
		case "y-dimension":
			if !inMediaSize || dimensions == nil || !validMediaColPositiveInteger(member) {
				return false
			}
			dimensions.hasY = true
		case "media-type", "media-source", "media-color":
			if !validMediaColKeyword(member) {
				return false
			}
		case "media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin":
			if !validMediaColNonNegativeInteger(member) {
				return false
			}
		default:
			return false
		}
	}
	return dimensions == nil || dimensions.hasX && dimensions.hasY
}

func validMediaSizeMember(attr goipp.Attribute, state *mediaColSyntaxState) bool {
	if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagBeginCollection {
		return false
	}
	collection, ok := attr.Values[0].V.(goipp.Collection)
	if !ok {
		return false
	}
	dimensions := mediaColDimensions{}
	return validMediaColMembers(goipp.Attributes(collection), state, true, &dimensions)
}

func validMediaColKeyword(attr goipp.Attribute) bool {
	if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagKeyword {
		return false
	}
	value, ok := attr.Values[0].V.(goipp.String)
	return ok && strings.TrimSpace(string(value)) != ""
}

func validMediaColPositiveInteger(attr goipp.Attribute) bool {
	if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagInteger {
		return false
	}
	value, ok := attr.Values[0].V.(goipp.Integer)
	return ok && value > 0
}

func validMediaColNonNegativeInteger(attr goipp.Attribute) bool {
	if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagInteger {
		return false
	}
	value, ok := attr.Values[0].V.(goipp.Integer)
	return ok && value >= 0
}

func attrsNamed(attrs goipp.Attributes, names ...string) goipp.Attributes {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[strings.ToLower(name)] = struct{}{}
	}
	var out goipp.Attributes
	for _, attr := range attrs {
		if _, ok := wanted[strings.ToLower(attr.Name)]; ok {
			out = append(out, attr)
		}
	}
	return out
}

func closestSupportedVersion(version goipp.Version) goipp.Version {
	switch {
	case version.Major() < 1:
		return goipp.MakeVersion(1, 0)
	case version.Major() == 1 && version.Minor() == 0:
		return goipp.MakeVersion(1, 0)
	case version.Major() == 1:
		return goipp.MakeVersion(1, 1)
	default:
		return goipp.MakeVersion(2, 0)
	}
}

// FilterRequestedPrinterAttributes implements the named-attribute and "all"
// forms used by mainstream IPP clients. media-col-database is intentionally
// expensive and is returned only when named explicitly, as required by the
// IPP Everywhere profile.
func FilterRequestedPrinterAttributes(attrs, operation goipp.Attributes) goipp.Attributes {
	requested, ok := iattr.Attr(operation, "requested-attributes")
	explicitMediaDatabase := false
	wanted := make(map[string]struct{}, len(requested.Values))
	all := !ok
	if ok {
		for _, value := range requested.Values {
			name, stringOK := value.V.(goipp.String)
			if !stringOK {
				continue
			}
			key := strings.ToLower(string(name))
			wanted[key] = struct{}{}
			all = all || key == "all"
			explicitMediaDatabase = explicitMediaDatabase || key == "media-col-database"
		}
	}
	out := make(goipp.Attributes, 0, len(attrs))
	for _, attr := range attrs {
		name := strings.ToLower(attr.Name)
		if name == "media-col-database" && !explicitMediaDatabase {
			continue
		}
		if all {
			out = append(out, attr)
			continue
		}
		if _, exact := wanted[name]; exact || requestedAttributeCategory(wanted, name) {
			out = append(out, attr)
		}
	}
	return out
}

func requestedAttributeCategory(wanted map[string]struct{}, name string) bool {
	if _, ok := wanted["printer-description"]; ok {
		for _, prefix := range []string{"printer-name", "printer-info", "printer-location", "printer-make-and-model", "printer-uri", "printer-uuid", "printer-more-info", "printer-dns-sd-name"} {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	if _, ok := wanted["printer-status"]; ok {
		switch name {
		case "printer-state", "printer-state-reasons", "printer-state-message", "printer-is-accepting-jobs", "queued-job-count":
			return true
		}
	}
	if _, ok := wanted["job-template"]; ok {
		_, included := jobTemplatePrinterAttributes[name]
		return included
	}
	return false
}

// jobTemplatePrinterAttributes is an explicit membership list for the IPP
// "job-template" requested-attributes category. Attribute-name suffixes are
// not sufficient here: operations-supported, ipp-versions-supported,
// compression-supported, and URI/security attributes are printer-description
// attributes even though their names end in "-supported".
var jobTemplatePrinterAttributes = map[string]struct{}{
	"copies-supported": {}, "copies-default": {},
	"cover-back-supported": {}, "cover-back-default": {},
	"cover-front-supported": {}, "cover-front-default": {},
	"feed-orientation-supported": {}, "feed-orientation-default": {},
	"finishings-supported": {}, "finishings-default": {},
	"imposition-template-supported": {}, "imposition-template-default": {},
	"insert-sheet-supported": {}, "insert-sheet-default": {},
	"job-hold-until-supported": {}, "job-hold-until-default": {},
	"job-priority-supported": {}, "job-priority-default": {},
	"job-sheets-supported": {}, "job-sheets-default": {},
	"media-supported": {}, "media-default": {},
	"media-col-supported": {}, "media-col-default": {}, "media-col-ready": {},
	"media-ready": {}, "media-size-supported": {},
	"media-type-supported": {}, "media-type-default": {},
	"media-source-supported": {}, "media-source-default": {},
	"multiple-document-handling-supported": {}, "multiple-document-handling-default": {},
	"number-up-supported": {}, "number-up-default": {},
	"number-up-layout-supported": {}, "number-up-layout-default": {},
	"orientation-requested-supported": {}, "orientation-requested-default": {},
	"output-bin-supported": {}, "output-bin-default": {},
	"page-delivery-supported": {}, "page-delivery-default": {},
	"page-ranges-supported":                      {},
	"presentation-direction-number-up-supported": {}, "presentation-direction-number-up-default": {},
	"print-color-mode-supported": {}, "print-color-mode-default": {},
	"print-content-optimize-supported": {}, "print-content-optimize-default": {},
	"print-quality-supported": {}, "print-quality-default": {},
	"print-scaling-supported": {}, "print-scaling-default": {},
	"printer-resolution-supported": {}, "printer-resolution-default": {},
	"proof-print-supported": {}, "proof-print-default": {},
	"sides-supported": {}, "sides-default": {},
}
