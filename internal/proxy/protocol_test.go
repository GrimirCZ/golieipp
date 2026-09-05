package proxy

import (
	"testing"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
)

func TestProtocolProcessorValidatesEnvelopeAndTarget(t *testing.T) {
	printerURI := "ipp://proxy.example/printers/office"
	valid := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 0)
	valid.Operation = append(iattr.BasicOperationAttrs(printerURI),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
	)
	valid.Job = goipp.Attributes{iattr.Integer("copies", 1)}

	tests := []struct {
		name       string
		mutate     func(*goipp.Message)
		hasPayload bool
		status     goipp.Status
	}{
		{name: "request id zero is valid", hasPayload: true},
		{name: "missing payload", status: goipp.StatusErrorBadRequest},
		{name: "wrong queue target", hasPayload: true, status: goipp.StatusErrorNotFound, mutate: func(m *goipp.Message) {
			m.Operation = iattr.SetAttr(m.Operation, iattr.URI("printer-uri", "ipp://proxy.example/printers/other"))
		}},
		{name: "duplicate operation attribute", hasPayload: true, status: goipp.StatusErrorBadRequest, mutate: func(m *goipp.Message) {
			m.Operation = append(m.Operation, iattr.URI("printer-uri", printerURI))
		}},
		{name: "media and media-col conflict", hasPayload: true, status: goipp.StatusErrorConflicting, mutate: func(m *goipp.Message) {
			m.Job = append(m.Job, iattr.Keyword("media", "iso_a4_210x297mm"), goipp.MakeAttribute("media-col", goipp.TagBeginCollection, goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
			}))
		}},
		{name: "invalid copies range", hasPayload: true, status: goipp.StatusErrorAttributesOrValues, mutate: func(m *goipp.Message) {
			m.Job = iattr.SetAttr(m.Job, iattr.Integer("copies", 0))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := *valid
			msg.Operation = valid.Operation.DeepCopy()
			msg.Job = valid.Job.DeepCopy()
			if test.mutate != nil {
				test.mutate(&msg)
			}
			err := ValidateProtocolRequest(&msg, printerURI, test.hasPayload)
			if test.status == 0 {
				if err != nil {
					t.Fatalf("valid request rejected: %v", err)
				}
				return
			}
			if err == nil || err.Status != test.status {
				t.Fatalf("got error %#v, want status %s", err, test.status)
			}
		})
	}
}

func TestProtocolProcessorValidatesEveryMediaColMember(t *testing.T) {
	printerURI := "ipp://proxy.example/printers/office"
	base := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 58)
	base.Operation = append(iattr.BasicOperationAttrs(printerURI),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")),
	)

	mediaSize := func(x, y int) goipp.Attribute {
		return goipp.MakeAttrCollection("media-size",
			iattr.Integer("x-dimension", x),
			iattr.Integer("y-dimension", y),
		)
	}

	tests := []struct {
		name       string
		collection goipp.Collection
		wantError  bool
	}{
		{
			name:       "named key only",
			collection: goipp.Collection{iattr.Keyword("media-size-name", "iso_a4_210x297mm")},
		},
		{
			name:       "media key only",
			collection: goipp.Collection{iattr.Keyword("media-key", "iso_a4_210x297mm")},
		},
		{
			name: "named with valid nested size",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				mediaSize(21000, 29700),
			},
		},
		{
			name: "named with malformed nested dimension tag",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				goipp.MakeAttrCollection("media-size",
					iattr.Keyword("x-dimension", "21000"),
					iattr.Integer("y-dimension", 29700),
				),
			},
			wantError: true,
		},
		{
			name: "key with nested dimension cardinality error",
			collection: goipp.Collection{
				iattr.Keyword("media-key", "iso_a4_210x297mm"),
				goipp.MakeAttrCollection("media-size",
					goipp.MakeAttr("x-dimension", goipp.TagInteger, goipp.Integer(21000), goipp.Integer(21590)),
					iattr.Integer("y-dimension", 29700),
				),
			},
			wantError: true,
		},
		{
			name: "named with non-positive nested dimension",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				mediaSize(0, 29700),
			},
			wantError: true,
		},
		{
			name: "named with duplicate nested dimension",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				goipp.MakeAttrCollection("media-size",
					iattr.Integer("x-dimension", 21000),
					iattr.Integer("x-dimension", 21590),
					iattr.Integer("y-dimension", 29700),
				),
			},
			wantError: true,
		},
		{
			name: "named with duplicate selector member",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				iattr.Keyword("media-size-name", "na_letter_8.5x11in"),
			},
			wantError: true,
		},
		{
			name: "named with malformed nested media-size value",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				iattr.Keyword("media-size", "not-a-collection"),
			},
			wantError: true,
		},
		{
			name: "named with unknown scalar member",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				iattr.Keyword("media-unknown", "value"),
			},
			wantError: true,
		},
		{
			name: "named with empty member name",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				goipp.MakeAttribute("", goipp.TagKeyword, goipp.String("value")),
			},
			wantError: true,
		},
		{
			name: "named with unknown nested collection member",
			collection: goipp.Collection{
				iattr.Keyword("media-size-name", "iso_a4_210x297mm"),
				goipp.MakeAttrCollection("media-unknown", iattr.Keyword("nested", "value")),
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := *base
			msg.Operation = base.Operation.DeepCopy()
			msg.Job = goipp.Attributes{goipp.MakeAttribute("media-col", goipp.TagBeginCollection, test.collection)}
			err := ValidateProtocolRequest(&msg, printerURI, true)
			if test.wantError {
				if err == nil || err.Status != goipp.StatusErrorAttributesOrValues {
					t.Fatalf("got error %#v, want attributes-or-values", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid media-col rejected: %v", err)
			}
		})
	}
}

func TestProtocolProcessorRequiresRequestedAttributesKeyword(t *testing.T) {
	printerURI := "ipp://proxy.example/printers/office"
	msg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobs, 59)
	msg.Operation = append(iattr.BasicOperationAttrs(printerURI),
		goipp.MakeAttribute("requested-attributes", goipp.TagName, goipp.String("job-id")),
	)
	if err := ValidateProtocolRequest(msg, printerURI, false); err == nil || err.Status != goipp.StatusErrorAttributesOrValues {
		t.Fatalf("requested-attributes with name tag accepted: %#v", err)
	}
}

func TestProtocolProcessorRequiresSendDocumentLastDocumentAndValidOverride(t *testing.T) {
	printerURI := "ipp://proxy.example/printers/office"
	send := goipp.NewRequest(goipp.DefaultVersion, goipp.OpSendDocument, 55)
	send.Operation = append(iattr.BasicOperationAttrs(printerURI), iattr.Integer("job-id", 4))
	if err := ValidateProtocolRequest(send, printerURI, true); err == nil || err.Status != goipp.StatusErrorBadRequest {
		t.Fatalf("missing last-document accepted: %#v", err)
	}

	send.Operation = append(send.Operation, iattr.Boolean("last-document", true))
	if err := ValidateProtocolRequest(send, printerURI, true); err != nil {
		t.Fatalf("valid Send-Document rejected: %v", err)
	}
	if err := ValidateProtocolRequest(send, printerURI, false); err != nil {
		t.Fatalf("valid no-data final Send-Document rejected: %v", err)
	}
	send.Operation = iattr.SetAttr(send.Operation, iattr.Boolean("last-document", false))
	if err := ValidateProtocolRequest(send, printerURI, false); err == nil || err.Status != goipp.StatusErrorBadRequest {
		t.Fatalf("no-data non-final Send-Document accepted: %#v", err)
	}

	badOverride := goipp.NewRequest(goipp.DefaultVersion, goipp.OpValidateJob, 56)
	badOverride.Operation = iattr.BasicOperationAttrs(printerURI)
	badOverride.Job = goipp.Attributes{goipp.MakeAttribute("overrides", goipp.TagBeginCollection, goipp.Collection{
		iattr.Keyword("media", "iso_a4_210x297mm"),
	})}
	if err := ValidateProtocolRequest(badOverride, printerURI, false); err == nil || err.Status != goipp.StatusErrorAttributesOrValues {
		t.Fatalf("override without selector accepted: %#v", err)
	}
}

func TestProtocolProcessorRequiresCanonicalJobURI(t *testing.T) {
	printerURI := "ipp://proxy.example/printers/office"
	for _, raw := range []string{
		printerURI + "/jobs/4/extra/5",
		printerURI + "/jobs/4?redirect=1",
		"ipp://proxy.example/printers/other/jobs/4",
	} {
		msg := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobAttributes, 57)
		msg.Operation = append(iattr.BasicOperationAttrs(""), iattr.URI("job-uri", raw))
		msg.Operation = iattr.DropAttrs(msg.Operation, "printer-uri")
		if err := ValidateProtocolRequest(msg, printerURI, false); err == nil || err.Status != goipp.StatusErrorNotFound {
			t.Fatalf("non-canonical job URI %q accepted: %#v", raw, err)
		}
	}
}

func TestProtocolProcessorRejectsInvalidGroupOrdering(t *testing.T) {
	printerURI := "ipp://proxy.example/printers/office"
	msg := goipp.NewMessageWithGroups(goipp.DefaultVersion, goipp.Code(goipp.OpValidateJob), 7, goipp.Groups{
		{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{iattr.Integer("copies", 1)}},
		{Tag: goipp.TagOperationGroup, Attrs: iattr.BasicOperationAttrs(printerURI)},
	})
	if err := ValidateProtocolRequest(msg, printerURI, false); err == nil || err.Status != goipp.StatusErrorBadRequest {
		t.Fatalf("invalid group order accepted: %#v", err)
	}
}

func TestClosestSupportedVersion(t *testing.T) {
	tests := map[goipp.Version]goipp.Version{
		goipp.MakeVersion(0, 9): goipp.MakeVersion(1, 0),
		goipp.MakeVersion(1, 0): goipp.MakeVersion(1, 0),
		goipp.MakeVersion(1, 7): goipp.MakeVersion(1, 1),
		goipp.MakeVersion(2, 1): goipp.MakeVersion(2, 0),
		goipp.MakeVersion(3, 0): goipp.MakeVersion(2, 0),
	}
	for input, want := range tests {
		if got := closestSupportedVersion(input); got != want {
			t.Errorf("closestSupportedVersion(%s) = %s, want %s", input, got, want)
		}
	}
}

func TestFilterRequestedPrinterAttributesRequiresExplicitMediaDatabase(t *testing.T) {
	attrs := goipp.Attributes{
		iattr.Name("printer-name", "Office"),
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		goipp.MakeAttribute("media-col-database", goipp.TagBeginCollection, goipp.Collection{}),
	}
	allRequest := goipp.Attributes{goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("all"))}
	filtered := FilterRequestedPrinterAttributes(attrs, allRequest)
	if _, ok := iattr.Attr(filtered, "media-col-database"); ok {
		t.Fatal("media-col-database returned for requested-attributes=all")
	}
	explicit := goipp.Attributes{goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("media-col-database"))}
	filtered = FilterRequestedPrinterAttributes(attrs, explicit)
	if _, ok := iattr.Attr(filtered, "media-col-database"); !ok {
		t.Fatal("explicitly requested media-col-database was omitted")
	}
}

func TestFilterRequestedPrinterAttributesUsesExplicitJobTemplateMembership(t *testing.T) {
	attrs := goipp.Attributes{
		iattr.Keyword("media-supported", "iso_a4_210x297mm"),
		iattr.Keyword("copies-default", "1"),
		iattr.Keyword("compression-supported", "none"),
		iattr.Keyword("ipp-versions-supported", "2.0"),
		iattr.Keyword("operations-supported", "print-job"),
		iattr.URI("printer-uri-supported", "ipp://proxy/printers/office"),
	}
	request := goipp.Attributes{goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("job-template"))}
	filtered := FilterRequestedPrinterAttributes(attrs, request)
	for _, name := range []string{"media-supported", "copies-default"} {
		if _, ok := iattr.Attr(filtered, name); !ok {
			t.Fatalf("job-template category omitted %q: %#v", name, filtered)
		}
	}
	for _, name := range []string{"compression-supported", "ipp-versions-supported", "operations-supported", "printer-uri-supported"} {
		if _, ok := iattr.Attr(filtered, name); ok {
			t.Fatalf("job-template category leaked %q: %#v", name, filtered)
		}
	}
}
