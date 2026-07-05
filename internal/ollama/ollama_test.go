package ollama

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeReachable(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/version" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte(`{"version":"0.x"}`))
	}))
	defer server.Close()

	probe := Probe{BaseURL: server.URL, Client: server.Client()}
	if err := probe.Reachable(); err != nil {
		test.Fatalf("expected reachable, got %v", err)
	}
}

func TestProbeUnreachable(test *testing.T) {
	// Nothing listening on this port.
	probe := Probe{BaseURL: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}
	if err := probe.Reachable(); err == nil {
		test.Fatal("expected an error when Ollama is unreachable")
	}
}

func TestProbeNon200(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	probe := Probe{BaseURL: server.URL, Client: server.Client()}
	if err := probe.Reachable(); err == nil {
		test.Fatal("expected an error on non-200 status")
	}
}

// HumanByteSize selects the right binary unit and rounds to one decimal. The
// boundary at exactly 1024 must roll over to the next unit ("1.0 KB", not
// "1024 B"), and sub-KB values keep the plain "N B" form.
func TestHumanByteSize(test *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},    // still bytes just below the unit
		{1024, "1.0 KB"},    // exact rollover to KB
		{1536, "1.5 KB"},    // rounds to one decimal
		{1048576, "1.0 MB"}, // 1024*1024
		{1610612736, "1.5 GB"},
		{1099511627776, "1.0 TB"},
	}
	for _, testCase := range cases {
		if got := HumanByteSize(testCase.bytes); got != testCase.want {
			test.Errorf("HumanByteSize(%d) = %q, want %q", testCase.bytes, got, testCase.want)
		}
	}
}
