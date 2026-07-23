package ollama

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceString covers the Source.String() rendering for both the fresh and
// cached values (and confirms any non-fresh value renders "cached").
func TestSourceString(test *testing.T) {
	if got := SourceFresh.String(); got != "fresh" {
		test.Errorf("SourceFresh.String() = %q, want fresh", got)
	}
	if got := SourceCached.String(); got != "cached" {
		test.Errorf("SourceCached.String() = %q, want cached", got)
	}
	if got := Source(99).String(); got != "cached" {
		test.Errorf("Source(99).String() = %q, want cached (non-fresh default)", got)
	}
}

// TestFakeClient exercises the in-memory Fake used across the CLI/TUI tests: it
// returns its configured values, records the names passed in, replays PullFrames
// through the callback, and honours the per-name PullErrs override.
func TestFakeClient(test *testing.T) {
	fake := &Fake{
		ListModels: []Model{{Name: "llama3.2:3b"}},
		ListErr:    errors.New("list boom"),
		PullFrames: []PullProgress{{Status: "pulling manifest"}, {Status: "success"}},
		PullErr:    errors.New("default pull err"),
		PullErrs:   map[string]error{"bad:tag": errors.New("per-name err")},
		RemoveErr:  errors.New("remove boom"),
		ShowInfo:   ModelInfo{Name: "gemma"},
		ShowErr:    errors.New("show boom"),
	}

	models, listErr := fake.List()
	if len(models) != 1 || models[0].Name != "llama3.2:3b" {
		test.Errorf("List models = %+v", models)
	}
	if listErr == nil || listErr.Error() != "list boom" {
		test.Errorf("List err = %v", listErr)
	}

	var frames []PullProgress
	pullErr := fake.Pull("good:tag", func(progress PullProgress) { frames = append(frames, progress) })
	if len(frames) != 2 || frames[1].Status != "success" {
		test.Errorf("replayed frames = %+v", frames)
	}
	if pullErr == nil || pullErr.Error() != "default pull err" {
		test.Errorf("Pull (no per-name override) err = %v", pullErr)
	}
	if fake.PulledName != "good:tag" {
		test.Errorf("PulledName = %q", fake.PulledName)
	}

	// The per-name override wins over PullErr for a matching name.
	if err := fake.Pull("bad:tag", nil); err == nil || err.Error() != "per-name err" {
		test.Errorf("per-name Pull err = %v", err)
	}
	if want := []string{"good:tag", "bad:tag"}; len(fake.PulledNames) != 2 ||
		fake.PulledNames[0] != want[0] || fake.PulledNames[1] != want[1] {
		test.Errorf("PulledNames = %v, want %v", fake.PulledNames, want)
	}

	if err := fake.Remove("llama3.2:3b"); err == nil || err.Error() != "remove boom" {
		test.Errorf("Remove err = %v", err)
	}
	if fake.RemovedName != "llama3.2:3b" {
		test.Errorf("RemovedName = %q", fake.RemovedName)
	}

	info, showErr := fake.Show("gemma")
	if info.Name != "gemma" || showErr == nil || showErr.Error() != "show boom" {
		test.Errorf("Show = %+v, %v", info, showErr)
	}
	if fake.ShownName != "gemma" {
		test.Errorf("ShownName = %q", fake.ShownName)
	}
}

// TestFakeClientZeroValue confirms the zero-value Fake is usable: empty/nil
// results, a nil progress callback, and no per-name map lookup.
func TestFakeClientZeroValue(test *testing.T) {
	fake := &Fake{}
	if models, err := fake.List(); models != nil || err != nil {
		test.Errorf("zero List = %v, %v", models, err)
	}
	if err := fake.Pull("x", nil); err != nil {
		test.Errorf("zero Pull err = %v", err)
	}
	if err := fake.Remove("x"); err != nil {
		test.Errorf("zero Remove err = %v", err)
	}
	if info, err := fake.Show("x"); info.Name != "" || err != nil {
		test.Errorf("zero Show = %+v, %v", info, err)
	}
}

// TestRealClientHonoursEnvBaseURL covers the RealClient constructor: OLLAMA_BASE_URL
// overrides the base (trailing slash trimmed) and an empty env falls back to
// DefaultBaseURL.
func TestRealClientHonoursEnvBaseURL(test *testing.T) {
	test.Setenv("OLLAMA_BASE_URL", "http://remote.ollama.test:11434/")
	client, ok := RealClient().(realClient)
	if !ok {
		test.Fatalf("RealClient did not return a realClient")
	}
	if client.baseURL != "http://remote.ollama.test:11434" {
		test.Errorf("baseURL = %q, want the env value with the trailing slash trimmed", client.baseURL)
	}
	if client.httpClient == nil {
		test.Error("httpClient must be set")
	}

	test.Setenv("OLLAMA_BASE_URL", "")
	fallback, ok := RealClient().(realClient)
	if !ok {
		test.Fatalf("RealClient did not return a realClient")
	}
	if fallback.baseURL != DefaultBaseURL {
		test.Errorf("baseURL = %q, want DefaultBaseURL when env is empty", fallback.baseURL)
	}
}

// TestRealProbeDefaults covers the RealProbe constructor.
func TestRealProbeDefaults(test *testing.T) {
	probe := RealProbe()
	if probe.BaseURL != DefaultBaseURL {
		test.Errorf("BaseURL = %q, want DefaultBaseURL", probe.BaseURL)
	}
	if probe.Client == nil || probe.Client.Timeout == 0 {
		test.Errorf("RealProbe must set a client with a timeout, got %+v", probe.Client)
	}
}

// TestProbeReachableDefaultsClient covers the nil-client default branch of
// Reachable: with a dead loopback base and no client set it falls back to
// http.DefaultClient and returns a transport error.
func TestProbeReachableDefaultsClient(test *testing.T) {
	probe := Probe{BaseURL: "http://127.0.0.1:1"} // nothing listening; nil Client
	if err := probe.Reachable(); err == nil {
		test.Fatal("expected a transport error using the default client")
	}
}

// TestNotFoundErrorMessage covers the NotFoundError renderer and its errors.As
// classification.
func TestNotFoundErrorMessage(test *testing.T) {
	err := error(&NotFoundError{Name: "ghost:1b"})
	if !strings.Contains(err.Error(), "ghost:1b") || !strings.Contains(err.Error(), "not found") {
		test.Errorf("NotFoundError.Error() = %q", err.Error())
	}
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		test.Error("errors.As must classify a NotFoundError")
	}
}

// TestUnreachableErrorAndClassification covers NewUnreachable / IsUnreachable /
// the unreachableError Error+Unwrap, including the negative cases.
func TestUnreachableErrorAndClassification(test *testing.T) {
	err := NewUnreachable()
	if !IsUnreachable(err) {
		test.Fatal("NewUnreachable must be classified as unreachable")
	}
	if !strings.Contains(err.Error(), "could not reach the server") {
		test.Errorf("unreachable Error() = %q", err.Error())
	}
	if unwrapped := errors.Unwrap(err); unwrapped == nil || !strings.Contains(unwrapped.Error(), "connection refused") {
		test.Errorf("Unwrap = %v, want the wrapped cause", unwrapped)
	}
	if IsUnreachable(nil) {
		test.Error("IsUnreachable(nil) must be false")
	}
	if IsUnreachable(errors.New("plain")) {
		test.Error("IsUnreachable(plain error) must be false")
	}
}

// TestStatusError covers both statusError branches directly: a non-empty body is
// included in the message; an empty (whitespace-only) body yields the bare status.
func TestStatusError(test *testing.T) {
	withBody := statusError(&http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader("upstream exploded")),
	})
	if !strings.Contains(withBody.Error(), "502") || !strings.Contains(withBody.Error(), "upstream exploded") {
		test.Errorf("statusError with body = %q", withBody.Error())
	}

	empty := statusError(&http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("   ")),
	})
	if !strings.Contains(empty.Error(), "unexpected status 500") {
		test.Errorf("statusError empty body = %q", empty.Error())
	}
}

// TestListErrorPaths covers List's non-200 (statusError) and JSON-decode-failure
// branches via a local httptest server.
func TestListErrorPaths(test *testing.T) {
	non200 := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("ollama is warming up"))
	}))
	defer non200.Close()
	if _, err := newTestClient(non200).List(); err == nil ||
		!strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "warming up") {
		test.Errorf("List non-200 err = %v", err)
	}

	badJSON := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("this is not json"))
	}))
	defer badJSON.Close()
	if _, err := newTestClient(badJSON).List(); err == nil || !strings.Contains(err.Error(), "decoding model list") {
		test.Errorf("List decode err = %v", err)
	}
}

// TestShowErrorPaths covers Show's 404 (NotFoundError), non-200 (statusError) and
// JSON-decode-failure branches.
func TestShowErrorPaths(test *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	_, err := newTestClient(notFound).Show("ghost")
	var missing *NotFoundError
	if !errors.As(err, &missing) {
		test.Errorf("Show 404 err = %v, want NotFoundError", err)
	}

	non200 := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte("kaboom"))
	}))
	defer non200.Close()
	if _, err := newTestClient(non200).Show("x"); err == nil ||
		!strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "kaboom") {
		test.Errorf("Show non-200 err = %v", err)
	}

	badJSON := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("{not json"))
	}))
	defer badJSON.Close()
	if _, err := newTestClient(badJSON).Show("x"); err == nil || !strings.Contains(err.Error(), "decoding model info") {
		test.Errorf("Show decode err = %v", err)
	}
}

// TestFetchLibraryIndexFailure covers FetchLibrary's index-fetch error branch (and
// fetchPage's non-200 branch): a 500 on /library aborts the whole scrape.
func TestFetchLibraryIndexFailure(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	models, err := FetchLibrary(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "fetch library index") {
		test.Fatalf("FetchLibrary index failure err = %v", err)
	}
	if models != nil {
		test.Errorf("models = %v, want nil on index failure", models)
	}
}

// TestFetchLibraryNoModelsParsed covers the "no models parsed" guard: a 200 index
// that contains no model anchors is treated as a failure.
func TestFetchLibraryNoModelsParsed(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/library" {
			_, _ = writer.Write([]byte(`<ul role="list"></ul>`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := FetchLibrary(context.Background(), server.Client(), server.URL); err == nil ||
		!strings.Contains(err.Error(), "no models parsed") {
		test.Fatalf("FetchLibrary no-models err = %v", err)
	}
}

// TestLoadCachedOrFetchLibraryScrapesWhenNoCache covers the cache-first path's
// no-cache branch: with no cache on disk it scrapes live, reports SourceFresh, and
// persists the cache for next time.
func TestLoadCachedOrFetchLibraryScrapesWhenNoCache(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)

	server := newLibraryServer(map[string]string{
		"llama3.2": tagRow("llama3.2", "1b", "1.3GB", "128K", "Text"),
		"qwen2.5":  tagRow("qwen2.5", "7b", "4.7GB", "32K", "Text"),
	})
	defer server.Close()

	models, source, err := LoadCachedOrFetchLibrary(context.Background(), server.Client(), server.URL)
	if err != nil {
		test.Fatalf("LoadCachedOrFetchLibrary: %v", err)
	}
	if source != SourceFresh {
		test.Errorf("source = %v, want fresh (no cache, live scrape)", source)
	}
	if len(models) != 2 {
		test.Fatalf("got %d models, want 2", len(models))
	}
	path := filepath.Join(home, ".ai-platform", "cache", "ollama-models.yaml")
	if _, statErr := os.Stat(path); statErr != nil {
		test.Fatalf("cache not persisted at %s: %v", path, statErr)
	}
}

// TestParseTagsNoMatchesReturnsNil covers the early return when a /tags body has no
// anchor for the requested model (a body that only mentions a different model).
func TestParseTagsNoMatchesReturnsNil(test *testing.T) {
	body := tagRow("other-model", "1b", "1.3GB", "128K", "Text")
	if tags := parseTags(body, "llama3.2"); tags != nil {
		test.Errorf("parseTags for an absent model = %v, want nil", tags)
	}
}

// TestParseTagsDedupesAcrossLayouts covers the de-duplication of the mobile+desktop
// anchor pair (first occurrence wins, order preserved) directly.
func TestParseTagsDedupesAcrossLayouts(test *testing.T) {
	body := tagRow("llama3.2", "latest", "2.0GB", "128K", "Text") +
		tagRow("llama3.2", "1b", "1.3GB", "128K", "Text")
	tags := parseTags(body, "llama3.2")
	if len(tags) != 2 {
		test.Fatalf("got %d tags, want 2 (deduped): %+v", len(tags), tags)
	}
	if tags[0].Name != "latest" || tags[1].Name != "1b" {
		test.Errorf("tag order = %q,%q want latest,1b", tags[0].Name, tags[1].Name)
	}
	if tags[0].Size != "2.0GB" || tags[0].Context != "128K" || tags[0].Input != "Text" {
		test.Errorf("first tag fields = %+v", tags[0])
	}
}

// TestLibraryPathLocation covers LibraryPath's happy path and the derived location.
func TestLibraryPathLocation(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	path, err := LibraryPath()
	if err != nil {
		test.Fatalf("LibraryPath: %v", err)
	}
	want := filepath.Join(home, ".ai-platform", "cache", "ollama-models.yaml")
	if path != want {
		test.Errorf("LibraryPath = %q, want %q", path, want)
	}
}

// TestLoadLibraryParseError covers LoadLibrary's YAML-parse-failure branch: a cache
// file with malformed YAML surfaces a parse error rather than panicking.
func TestLoadLibraryParseError(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	path := filepath.Join(home, ".ai-platform", "cache", "ollama-models.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatalf("mkdir: %v", err)
	}
	// A YAML mapping where a list is expected fails to unmarshal into []LibraryModel.
	if err := os.WriteFile(path, []byte("not: [a, valid, list\n"), 0o644); err != nil {
		test.Fatalf("write: %v", err)
	}
	if _, err := LoadLibrary(); err == nil || !strings.Contains(err.Error(), "load library") {
		test.Errorf("LoadLibrary parse err = %v", err)
	}
}
