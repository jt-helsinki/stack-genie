package egress

import "testing"

// TestSplitHostPort covers the default-port path, an explicit host:port, and
// every error branch (leading colon = empty host, trailing colon, non-numeric
// port).
func TestSplitHostPort(test *testing.T) {
	cases := []struct {
		input    string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		// Bare host: no colon → default HTTPS port 443.
		{"api.github.com", "api.github.com", 443, false},
		// Wildcard suffix, bare → 443.
		{"*.npmjs.org", "*.npmjs.org", 443, false},
		// The gateway token, bare → 443.
		{"gateway", "gateway", 443, false},
		// Surrounding whitespace is trimmed.
		{"  api.github.com  ", "api.github.com", 443, false},
		// Explicit host:port.
		{"db.internal:8080", "db.internal", 8080, false},
		{"gateway:5432", "gateway", 5432, false},
		// Leading colon → empty host → error.
		{":8080", "", 0, true},
		// Trailing colon → missing port → error.
		{"host:", "", 0, true},
		// Non-numeric port → error.
		{"host:abc", "", 0, true},
	}
	for _, testCase := range cases {
		host, port, err := SplitHostPort(testCase.input)
		if testCase.wantErr {
			if err == nil {
				test.Errorf("SplitHostPort(%q) = (%q,%d,nil), want error", testCase.input, host, port)
			}
			if host != "" || port != 0 {
				test.Errorf("SplitHostPort(%q) on error = (%q,%d), want (\"\",0)", testCase.input, host, port)
			}
			continue
		}
		if err != nil {
			test.Errorf("SplitHostPort(%q) unexpected error: %v", testCase.input, err)
			continue
		}
		if host != testCase.wantHost || port != testCase.wantPort {
			test.Errorf("SplitHostPort(%q) = (%q,%d), want (%q,%d)",
				testCase.input, host, port, testCase.wantHost, testCase.wantPort)
		}
	}
}
