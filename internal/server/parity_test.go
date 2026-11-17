package server

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Parity replays the goldens captured from the Rust server (testdata/README.txt
// in internal/store) in capture order against a fresh app.
//
// Only goldens whose request carries no placeholder can be replayed: the
// others were normalised (tokens, cookies, codes) and the hidden requests that
// created those secrets are not committed. They are covered by the handler
// tests of each area, which are ported from the Rust tests.

// parityExcluded are replayable goldens that still cannot match a fresh app,
// with the reason.
var parityExcluded = map[string]string{
	"admin_login_bad_json_400":             "the JSON parser's wording differs (status matches); deviation noted in step 9",
	"admin_login_missing_field_422":        "line and column in the serde message differ (status matches)",
	"admin_login_ok_alice":                 "needs the user alice, created by a hidden request",
	"oauth_login_flow_ok_alice_android":    "needs the user alice, created by a hidden request",
	"admin_login_throttle_locked_429":      "needs the failed logins of a hidden request loop",
	"oauth_login_flow_throttle_shared_429": "needs the failed logins of a hidden request loop",
}

const paritySetupCode = "CD42Y-6YC3Y"

var normalizers = []struct {
	pattern *regexp.Regexp
	to      string
}{
	{regexp.MustCompile(`domus_r_[0-9a-f]{64}`), "<REFRESH_TOKEN>"},
	{regexp.MustCompile(`domus_[0-9a-f]{32}`), "<TOKEN>"},
	{regexp.MustCompile(`(^|[^0-9a-f])[0-9a-f]{64}($|[^0-9a-f])`), "${1}<HEX64>${2}"},
	{regexp.MustCompile(`(^|[^0-9a-f])[0-9a-f]{32}($|[^0-9a-f])`), "${1}<HEX32>${2}"},
	{regexp.MustCompile(`(^|[^0-9a-f])[0-9a-f]{26}($|[^0-9a-f])`), "${1}<HEX26>${2}"},
	{regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`), "<TS>"},
	{regexp.MustCompile(`in \d+ seconds`), "in <N> seconds"},
}

func normalizeText(text string) string {
	for _, rule := range normalizers {
		text = rule.pattern.ReplaceAllString(text, rule.to)
	}
	return text
}

func TestTheRustGoldensReplayAgainstAFreshApp(t *testing.T) {
	app := newTestAppWith(t, func(cfg *Config) { cfg.SetupCode = paritySetupCode })
	order, err := os.ReadFile(filepath.Join("testdata", "rust-order.txt"))
	if err != nil {
		t.Fatal(err)
	}
	replayed := 0
	for _, name := range strings.Fields(string(order)) {
		g := loadGolden(t, name)
		if !replayable(t, g) {
			continue
		}
		if reason, ok := parityExcluded[name]; ok {
			t.Logf("%s: not replayed: %s", name, reason)
			continue
		}
		replayed++
		t.Run(name, func(t *testing.T) { replayGolden(t, app, name, g) })
	}
	if replayed < 50 {
		t.Fatalf("only %d goldens replayed; the fixtures changed", replayed)
	}
	t.Logf("replayed %d of the goldens", replayed)
}

// replayable is true when the request has no normalised value.
func replayable(t *testing.T, g golden) bool {
	t.Helper()
	raw, err := json.Marshal(g.Request)
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal escapes "<" as \u003c.
	return !bytes.Contains(raw, []byte(`\u003c`))
}

func replayGolden(t *testing.T, app *testApp, name string, g golden) {
	req := request{method: g.Request.Method, path: g.Request.Path, headers: g.Request.Headers}
	switch {
	case g.Request.BodyText != nil:
		req.body = *g.Request.BodyText
	case g.Request.Form != nil:
		req.body = url.Values(formValues(g.Request.Form)).Encode()
	case g.Request.Body != nil:
		req.body = string(g.Request.Body)
	}
	rec := app.do(t, req)
	want := g.Response
	if rec.Code != want.Status {
		t.Errorf("%s: status %d, want %d (body %s)", name, rec.Code, want.Status, rec.Body)
	}
	for header, value := range want.Headers {
		got := normalizeHeader(header, rec.Header().Values(header))
		if got != value {
			t.Errorf("%s: header %s = %q, want %q", name, header, got, value)
		}
	}
	checkNormalizedBody(t, name, rec.Body.Bytes(), want)
}

// normalizeHeader joins the values like the capture did and applies the
// normalisation rules that touch headers.
func normalizeHeader(name string, values []string) string {
	joined := strings.Join(values, ", ")
	if strings.EqualFold(name, "retry-after") && joined != "" {
		return "<N>"
	}
	return normalizeText(strings.ReplaceAll(joined, goldenVersion, "<VERSION>"))
}

func checkNormalizedBody(t *testing.T, name string, got []byte, want goldenResponse) {
	t.Helper()
	if want.Body == nil {
		checkGoldenBody(t, name, got, want)
		return
	}
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Errorf("%s: body is not JSON: %s", name, got)
		return
	}
	normalized, err := json.Marshal(normalizeJSON("", gotValue))
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, normalized, want.Body) {
		t.Errorf("%s: body %s, want %s", name, normalized, want.Body)
	}
}

var unixKeys = map[string]bool{"created": true, "last_seen": true, "last_used": true, "expires": true, "elevated_until": true}

// normalizeJSON applies the body rules: strings are rewritten, and integers
// under the time keys become "<UNIX>" (null stays null).
func normalizeJSON(key string, value any) any {
	switch v := value.(type) {
	case string:
		return normalizeText(v)
	case float64:
		if unixKeys[key] {
			return "<UNIX>"
		}
	case []any:
		for i := range v {
			v[i] = normalizeJSON(key, v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = normalizeJSON(k, v[k])
		}
	}
	return value
}

func formValues(form map[string]string) map[string][]string {
	values := make(map[string][]string, len(form))
	for key, value := range form {
		values[key] = []string{value}
	}
	return values
}
