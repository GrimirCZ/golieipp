package ipp

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/OpenPrinting/goipp"
)

const A4XDimension = 21000
const A4YDimension = 29700

func Attr(attrs goipp.Attributes, name string) (goipp.Attribute, bool) {
	for _, attr := range attrs {
		if strings.EqualFold(attr.Name, name) {
			return attr, true
		}
	}
	return goipp.Attribute{}, false
}

func FirstString(attrs goipp.Attributes, name string) (string, bool) {
	attr, ok := Attr(attrs, name)
	if !ok || len(attr.Values) == 0 {
		return "", false
	}
	switch v := attr.Values[0].V.(type) {
	case goipp.String:
		return string(v), true
	default:
		return fmt.Sprint(v), true
	}
}

func FirstInt(attrs goipp.Attributes, name string) (int, bool) {
	attr, ok := Attr(attrs, name)
	if !ok || len(attr.Values) == 0 {
		return 0, false
	}
	switch v := attr.Values[0].V.(type) {
	case goipp.Integer:
		return int(v), true
	default:
		return 0, false
	}
}

func HasStringValue(attrs goipp.Attributes, name, value string) bool {
	attr, ok := Attr(attrs, name)
	if !ok {
		return false
	}
	for _, val := range attr.Values {
		if s, ok := val.V.(goipp.String); ok && string(s) == value {
			return true
		}
	}
	return false
}

func SetAttr(attrs goipp.Attributes, attr goipp.Attribute) goipp.Attributes {
	out := make(goipp.Attributes, 0, len(attrs)+1)
	replaced := false
	for _, existing := range attrs {
		if strings.EqualFold(existing.Name, attr.Name) {
			if !replaced {
				out = append(out, attr)
				replaced = true
			}
			continue
		}
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, attr)
	}
	return out
}

func DropAttrs(attrs goipp.Attributes, names ...string) goipp.Attributes {
	drop := map[string]struct{}{}
	for _, name := range names {
		drop[strings.ToLower(name)] = struct{}{}
	}
	out := make(goipp.Attributes, 0, len(attrs))
	for _, attr := range attrs {
		if _, ok := drop[strings.ToLower(attr.Name)]; ok {
			continue
		}
		out = append(out, attr)
	}
	return out
}

func Keyword(name, value string) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagKeyword, goipp.String(value))
}

func Keywords(name string, values ...string) goipp.Attribute {
	if len(values) == 0 {
		return goipp.Attribute{Name: name}
	}
	ippValues := make([]goipp.Value, len(values))
	for i, value := range values {
		ippValues[i] = goipp.String(value)
	}
	return goipp.MakeAttr(name, goipp.TagKeyword, ippValues[0], ippValues[1:]...)
}

func URI(name, value string) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagURI, goipp.String(value))
}

func Name(name, value string) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagName, goipp.String(value))
}

func Text(name, value string) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagText, goipp.String(value))
}

func Boolean(name string, value bool) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagBoolean, goipp.Boolean(value))
}

func Integer(name string, value int) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(value))
}

func A4MediaCol(mediaType string) goipp.Attribute {
	mediaSize := goipp.MakeAttrCollection("media-size",
		goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(A4XDimension)),
		goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(A4YDimension)),
	)
	if mediaType == "" {
		return goipp.MakeAttrCollection("media-col", mediaSize)
	}
	return goipp.MakeAttrCollection("media-col", mediaSize, Keyword("media-type", mediaType))
}

func A4MediaColNamed(name, mediaType string) goipp.Attribute {
	attr := A4MediaCol(mediaType)
	attr.Name = name
	return attr
}

func RewriteRequestPrinterURI(msg *goipp.Message, printerURI string) {
	msg.Operation = SetAttr(msg.Operation, URI("printer-uri", printerURI))
	msg.Groups = nil
}

func HTTPURLFromIPP(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "ipp":
		u.Scheme = "http"
	case "ipps":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported upstream URI scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func BasicOperationAttrs(uri string) goipp.Attributes {
	return goipp.Attributes{
		goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")),
		URI("printer-uri", uri),
	}
}
