package main

import "testing"

func TestExtractSubdomain(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
		ok   bool
	}{
		{name: "valid host", host: "demo.tunnel.com", want: "demo", ok: true},
		{name: "valid host with port", host: "demo.tunnel.com:8080", want: "demo", ok: true},
		{name: "invalid host", host: "localhost", want: "", ok: false},
		{name: "invalid subdomain", host: "Bad!.tunnel.com", want: "", ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := extractSubdomain(test.host)
			if got != test.want || ok != test.ok {
				t.Fatalf("extractSubdomain(%q) = (%q, %v), want (%q, %v)", test.host, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestPreviewBody(t *testing.T) {
	textView := previewBody([]byte("hello"))
	if textView.Encoding != "text" || textView.Content != "hello" || textView.Truncated {
		t.Fatalf("unexpected text preview: %+v", textView)
	}

	binaryView := previewBody([]byte{0xff, 0x00, 0x01})
	if binaryView.Encoding != "base64" || binaryView.Content == "" {
		t.Fatalf("unexpected binary preview: %+v", binaryView)
	}
}
