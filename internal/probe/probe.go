// Package probe asks a running engine a numeric question over HTTP: how
// busy is it (BUSY) and how far along its load is (PROGRESS). One probe
// shape serves both (ADR-0002, ADR-0005).
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
)

// maxBody bounds a probe response. A health or metrics endpoint never
// legitimately answers with more than this, and a stuck server streaming
// garbage must not eat memory.
const maxBody = 64 << 10

// Probe is a parsed "url#selector" value.
type Probe struct {
	URL      string
	Selector *config.Selector

	client *http.Client
}

// Parse splits "url#selector". Missing URL or selector is an error: a probe
// that cannot say where to ask is misconfigured, not empty.
func Parse(value string) (*Probe, error) {
	u, sel, found := strings.Cut(value, "#")
	if u == "" || !found || strings.TrimSpace(sel) == "" {
		return nil, fmt.Errorf("probe %q: want url#selector", value)
	}
	s, err := config.ParseSelector(sel)
	if err != nil {
		return nil, err
	}
	return &Probe{URL: u, Selector: s, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Result classifies one probe answer (ADR-0002): Free is count zero, Busy is
// any count above zero, Unknown means the probe could not answer. Unknown is
// never treated as free by callers: the drain fails closed.
type Result struct {
	Count int
	State State
	// Err carries the cause when State is Unknown, so failure reports can
	// name the last probe error (ADR-0005 actionable failures).
	Err error
}

// State is the typed probe outcome.
type State int

const (
	// Free means the probe answered with a count of zero.
	Free State = iota
	// Busy means the probe answered with a count above zero.
	Busy
	// Unknown means the probe errored: HTTP failure, non-JSON body, missing
	// field, or a non-number where a count belongs.
	Unknown
)

// Count fetches the URL and resolves the selector to a number. Any failure
// is Unknown: callers must not read a broken probe as an empty queue.
func (p *Probe) Count(ctx context.Context) (int, error) {
	n, err := p.fetchNumber(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ProgressValue fetches the URL and resolves the selector to a loading
// progress. Nil means the endpoint answered but carries no usable numeric
// progress; an error means the probe itself failed. Message-only changes
// never count as progress (ADR-0005), so only numbers are returned.
func (p *Probe) ProgressValue(ctx context.Context) (*float64, error) {
	body, err := p.fetch(ctx)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("probe %s: not JSON", p.URL)
	}
	v, ok := lookup(doc, p.Selector.Path())
	if !ok {
		return nil, nil
	}
	f, ok := toFloat(v)
	if !ok {
		return nil, nil
	}
	return &f, nil
}

// Check runs one probe and classifies it.
func (p *Probe) Check(ctx context.Context) Result {
	n, err := p.Count(ctx)
	if err != nil {
		return Result{State: Unknown, Err: err}
	}
	if n == 0 {
		return Result{Count: 0, State: Free}
	}
	return Result{Count: n, State: Busy}
}

// fetch GETs the probe URL with a bounded body. Any HTTP answer (even
// non-200) is returned: during a model load, endpoints commonly serve
// progress in a 503 body, and a 503 without the selector still classifies
// as Unknown, never as Free.
func (p *Probe) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("probe %s: body over %d bytes", p.URL, maxBody)
	}
	return body, nil
}

// fetchNumber resolves the selector to a number; anything else (missing
// field, string, object) is an Unknown error.
func (p *Probe) fetchNumber(ctx context.Context) (int, error) {
	body, err := p.fetch(ctx)
	if err != nil {
		return 0, err
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, fmt.Errorf("probe %s: not JSON", p.URL)
	}
	v, ok := lookup(doc, p.Selector.Path())
	if !ok {
		return 0, fmt.Errorf("probe %s: selector %s not found", p.URL, p.Selector)
	}
	f, ok := toFloat(v)
	if !ok {
		return 0, fmt.Errorf("probe %s: selector %s is not a number", p.URL, p.Selector)
	}
	// A fractional count is not a count: truncating 0.4 to 0 would answer
	// "free" for an engine that is still serving, and the drain fails open.
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("probe %s: selector %s is not an integer count: %v", p.URL, p.Selector, f)
	}
	return int(f), nil
}

// lookup walks a decoded JSON document along a selector path.
func lookup(doc any, path []string) (any, bool) {
	cur := doc
	for _, tok := range path {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			if tok == "length" {
				return float64(len(node)), true
			}
			idx, err := strconvAtoi(tok)
			if err != nil {
				return nil, false
			}
			if idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		case string:
			if tok == "length" {
				return float64(len(node)), true
			}
			return nil, false
		default:
			return nil, false
		}
	}
	return cur, true
}

// toFloat accepts JSON numbers (and numeric strings are deliberately
// rejected: "half" is not a progress value).
func toFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func strconvAtoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty index")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an index: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
