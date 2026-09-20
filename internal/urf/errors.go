package urf

import (
	"context"
	"errors"
	"fmt"
)

// ErrorKind identifies the class of failure returned by Translate.
type ErrorKind uint8

const (
	// KindInvalidOptions means that Options cannot describe a supported
	// translation or contains an invalid limit.
	KindInvalidOptions ErrorKind = iota + 1
	// KindMalformedInput means that the URF stream is truncated or has an
	// invalid structural value.
	KindMalformedInput
	// KindUnsupportedInput means that the stream is structurally readable but
	// uses a color/depth/layout outside the supported exact mappings.
	KindUnsupportedInput
	// KindSettingsMismatch means that a valid page conflicts with normalized
	// policy settings supplied by the caller.
	KindSettingsMismatch
	// KindResourceLimit means that a configured resource bound was exceeded.
	KindResourceLimit
	// KindSourceIO means that reading the source failed.
	KindSourceIO
	// KindDestinationIO means that writing, seeking, or rewinding the
	// destination failed.
	KindDestinationIO
	// KindCanceled means that ctx was canceled or reached its deadline.
	KindCanceled
)

func (k ErrorKind) String() string {
	switch k {
	case KindInvalidOptions:
		return "invalid options"
	case KindMalformedInput:
		return "malformed input"
	case KindUnsupportedInput:
		return "unsupported input"
	case KindSettingsMismatch:
		return "settings mismatch"
	case KindResourceLimit:
		return "resource limit"
	case KindSourceIO:
		return "source I/O"
	case KindDestinationIO:
		return "destination I/O"
	case KindCanceled:
		return "canceled"
	default:
		return "unknown error"
	}
}

// Error is the typed error returned by Translate.  Page and Row are one-based
// when known; Offset is the number of source bytes consumed before the
// failing operation and is -1 when no useful offset is available.
//
// Err is retained with Unwrap so callers can use errors.Is/errors.As for
// context cancellation and underlying I/O failures.
type Error struct {
	Kind   ErrorKind
	Op     string
	Page   uint32
	Row    uint32
	Offset int64
	Err    error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}

	prefix := e.Kind.String()
	if e.Op != "" {
		prefix += ": " + e.Op
	}
	if e.Page != 0 {
		prefix += fmt.Sprintf(" (page %d", e.Page)
		if e.Row != 0 {
			prefix += fmt.Sprintf(", row %d", e.Row)
		}
		prefix += ")"
	} else if e.Row != 0 {
		prefix += fmt.Sprintf(" (row %d)", e.Row)
	}
	if e.Err != nil {
		return prefix + ": " + e.Err.Error()
	}
	return prefix
}

// Unwrap returns the underlying source, destination, or context error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ErrorKindOf returns the kind carried by err, or zero if err is not an
// *Error produced by this package.
func ErrorKindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e.Kind
	}
	return 0
}

func canceledError(ctx context.Context, op string, offset int64, page, row uint32) error {
	err := ctx.Err()
	if err == nil {
		err = context.Canceled
	}
	return &Error{Kind: KindCanceled, Op: op, Offset: offset, Page: page, Row: row, Err: err}
}

func wrapError(kind ErrorKind, op string, err error, offset int64, page, row uint32) error {
	if err == nil {
		err = errors.New(kind.String())
	}
	return &Error{Kind: kind, Op: op, Err: err, Offset: offset, Page: page, Row: row}
}
