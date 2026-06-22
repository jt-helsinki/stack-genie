package cli

import "testing"

func TestSplitHostPort(test *testing.T) {
	cases := []struct {
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		// Bare host -> default HTTPS port.
		{"api.github.com", "api.github.com", 443, false},
		{"registry.npmjs.org", "registry.npmjs.org", 443, false},
		{"gateway", "gateway", 443, false},
		{"10.0.0.5", "10.0.0.5", 443, false},
		// Bare wildcard -> default HTTPS port.
		{"*.npmjs.org", "*.npmjs.org", 443, false},
		// Explicit host:port, split on the last colon.
		{"db.internal:5432", "db.internal", 5432, false},
		{"*.npmjs.org:8443", "*.npmjs.org", 8443, false},
		{"gateway:5442", "gateway", 5442, false},
		// Errors: leading colon, trailing colon, non-numeric port.
		{":443", "", 0, true},
		{"host:", "", 0, true},
		{"host:notaport", "", 0, true},
	}
	for _, testCase := range cases {
		host, port, err := splitHostPort(testCase.input)
		if testCase.wantErr {
			if err == nil {
				test.Errorf("splitHostPort(%q) = (%q,%d,nil), want error", testCase.input, host, port)
			}
			continue
		}
		if err != nil {
			test.Errorf("splitHostPort(%q) unexpected error: %v", testCase.input, err)
			continue
		}
		if host != testCase.wantHost || port != testCase.wantPort {
			test.Errorf("splitHostPort(%q) = (%q,%d), want (%q,%d)",
				testCase.input, host, port, testCase.wantHost, testCase.wantPort)
		}
	}
}
