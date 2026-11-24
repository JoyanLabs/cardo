package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCountReturnsFieldValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"live":{"connections":2}}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.live.connections")
	n, err := p.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestCountMissingFieldIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"error":{"message":"loading"}}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.live.connections")
	if _, err := p.Count(context.Background()); err == nil {
		t.Fatal("want error for missing field (unknown, never free)")
	}
}

func TestCountHTTPOrNonJSONIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json at all"))
	}))
	defer srv.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"starting"}`))
	}))
	defer srv2.Close()

	for name, p := range map[string]*Probe{
		"non-json": mustProbe(t, srv.URL+"#$.x"),
		"http-503": mustProbe(t, srv2.URL+"#$.x"),
	} {
		if _, err := p.Count(context.Background()); err == nil {
			t.Errorf("%s: want unknown error", name)
		}
	}
}

func TestCountTopLevelField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"generations_active":0}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#generations_active")
	n, err := p.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

func TestCountArrayLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"queue":{"running":[{"a":1}],"pending":[{"a":1},{"a":2}]}}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.queue.pending.length")
	n, err := p.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestCountNonNumberIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"progress":"half"}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.progress")
	if _, err := p.Count(context.Background()); err == nil {
		t.Fatal("string progress is not a count: want error")
	}
}

// TestCountFractionalIsUnknown pins the truncation guard: a fractional count
// is not a zero. int(0.4) == 0 would classify a half-loaded engine as Free
// and let the drain swap over an engine that is still serving.
func TestCountFractionalIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"live":{"connections":0.4}}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.live.connections")
	if _, err := p.Count(context.Background()); err == nil {
		t.Fatal("a fractional count must be an error, not a truncated 0")
	}
	res := p.Check(context.Background())
	if res.State != Unknown {
		t.Errorf("Check = %v, want Unknown (a fractional count is never Free)", res.State)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not an integer count") {
		t.Errorf("the error must name the shape it rejected, got %v", res.Err)
	}
}

// TestCountIntegralFloatIsAccepted: the guard is about the fraction, not the
// JSON type. An endpoint that reports 2.0 for two connections is a count.
func TestCountIntegralFloatIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"live":{"connections":2.0}}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.live.connections")
	n, err := p.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestProgressValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"progress":0.25,"message":"loading weights"}`))
	}))
	defer srv.Close()

	p := mustProbe(t, srv.URL+"#$.progress")
	got, err := p.ProgressValue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != 0.25 {
		t.Errorf("ProgressValue = %v, want 0.25", got)
	}
}

func mustProbe(t *testing.T, value string) *Probe {
	t.Helper()
	p, err := Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
