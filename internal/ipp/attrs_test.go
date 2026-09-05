package ipp

import "testing"

func TestHTTPURLFromIPPAddsDefaultPort(t *testing.T) {
	tests := map[string]string{
		"ipp://printer.example/ipp/print":       "http://printer.example:631/ipp/print",
		"ipps://printer.example/ipp/print":      "https://printer.example:631/ipp/print",
		"ipp://printer.example:8631/ipp/print":  "http://printer.example:8631/ipp/print",
		"ipp://[2001:db8::1]/ipp/print":         "http://[2001:db8::1]:631/ipp/print",
		"http://printer.example:8080/ipp/print": "http://printer.example:8080/ipp/print",
	}
	for input, want := range tests {
		got, err := HTTPURLFromIPP(input)
		if err != nil {
			t.Fatalf("HTTPURLFromIPP(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("HTTPURLFromIPP(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHTTPURLFromIPPRejectsIncompleteURI(t *testing.T) {
	for _, input := range []string{"ipp:///ipp/print", "printer.example/ipp/print", "ftp://printer.example/print"} {
		if _, err := HTTPURLFromIPP(input); err == nil {
			t.Errorf("HTTPURLFromIPP(%q) unexpectedly succeeded", input)
		}
	}
}
