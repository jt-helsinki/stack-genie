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
