package obs

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// scrape fetches all metric families as exposition text.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.HTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestMetricsScrape(t *testing.T) {
	m, err := NewMetrics("v0.0.0-test", "deadbeef")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	// Instrument everything through the setter helpers.
	m.RecordReconcile(2*time.Second, ResultOK)
	m.RecordReconcile(time.Second, ResultError)
	m.SetServiceDrift("core/top-controller", true)
	m.SetServiceDrift("observability/prometheus", false)
	m.RecordDeploy("core/top-controller", 42*time.Second, ResultOK)
	m.SetLastSuccessfulReconcileAt(time.Unix(1700000000, 0))
	m.RecordWebhookDelivery("push", ResultOK)
	m.RecordGitPoll(3*time.Second, ResultError)
	m.ObserveSQLiteOperation("get", 5*time.Millisecond)

	out := scrape(t, m)

	for _, fragment := range []string{
		`top_reconcile_duration_seconds`,
		`top_reconcile_total{result="ok"} 1`,
		`top_reconcile_total{result="error"} 1`,
		`top_service_drift{service="core/top-controller"} 1`,
		`top_service_drift{service="observability/prometheus"} 0`,
		`top_deploy_total{result="ok",service="core/top-controller"} 1`,
		`top_deploy_duration_seconds_sum{service="core/top-controller"}`,
		`top_last_successful_reconcile_timestamp`,
		`top_webhook_deliveries_total{event="push",result="ok"} 1`,
		`top_git_poll_duration_seconds`,
		`top_git_poll_total{result="error"} 1`,
		`top_sqlite_operation_duration_seconds_sum{operation="get"}`,
	} {
		if !strings.Contains(out, fragment) {
			t.Errorf("/metrics missing %q", fragment)
		}
	}

	// build_info set once at construction, labels only version+commit.
	if !strings.Contains(out, `top_build_info{`) || !strings.Contains(out, "1") {
		t.Errorf("build_info missing: %s", families(out, "top_build_info"))
	}

	// Go collector present (cross-platform); process collector stays
	// optional — it must never fail the scrape with an error page.
	if !strings.Contains(out, "go_goroutines") {
		t.Error("Go collector not registered")
	}
	if !strings.Contains(out, "# EOF") && !strings.Contains(out, "# HELP") {
		t.Errorf("malformed exposition output")
	}
}

// families returns the lines of one metric family from exposition text.
func families(out, name string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, "# HELP "+name) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// Serving: the controller owns the listener; obs only registers handlers.
func TestServeMetricsOnGivenMux(t *testing.T) {
	m, err := NewMetrics("test", "abc123")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	mux := http.NewServeMux()
	m.ServeMetrics(mux)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/metrics"); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("/metrics via mux: %d", rec.Code)
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d %d", rec.Code, rec.Body.Len())
	}

	// The controller's mux stays free to serve other routes.
	mux.HandleFunc("/other", func(http.ResponseWriter, *http.Request) {})
	if rec := get("/other"); rec.Code != http.StatusOK {
		t.Errorf("mux coexistence broken: %d", rec.Code)
	}
}

// Label-set assertion: every registered top_ metric family's label set must
// match the ADR-0010 contract exactly, so an accidental high-cardinality
// label (sha, error string, URL, delivery id) cannot sneak in.
func TestLabelSetsBoundedByEnumerations(t *testing.T) {
	m, err := NewMetrics("test", "abc123")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	// Touch every vec so its family appears in the scrape.
	m.RecordReconcile(time.Millisecond, ResultOK)
	m.RecordReconcile(time.Millisecond, ResultDrift)
	m.RecordReconcile(time.Millisecond, ResultError)
	m.SetServiceDrift("a/b", true)
	m.RecordDeploy("a/b", time.Millisecond, ResultOK)
	m.SetLastSuccessfulReconcileAt(time.Now())
	m.RecordWebhookDelivery("push", ResultOK)
	m.RecordGitPoll(time.Millisecond, ResultOK)
	m.ObserveSQLiteOperation("get", time.Millisecond)

	out := scrape(t, m)

	want := map[string][]string{
		"top_reconcile_duration_seconds":          nil,
		"top_reconcile_total":                     {"result"},
		"top_service_drift":                       {"service"},
		"top_deploy_total":                        {"service", "result"},
		"top_deploy_duration_seconds":             {"service"},
		"top_last_successful_reconcile_timestamp": nil,
		"top_webhook_deliveries_total":            {"event", "result"},
		"top_git_poll_duration_seconds":           nil,
		"top_git_poll_total":                      {"result"},
		"top_sqlite_operation_duration_seconds":   {"operation"},
		"top_build_info":                          {"commit", "version"},
	}

	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue // HELP/TYPE metadata, noise
		}
		name := line
		if i := strings.IndexByte(line, '{'); i >= 0 {
			name = line[:i]
		} else if j := strings.IndexByte(line, ' '); j >= 0 {
			name = line[:j] // bare sample: name + value, no labels
		}
		base := name
		// Histogram buckets add the conventional `le` label; the family
		// itself is checked via its _sum/_count lines below.
		if strings.HasSuffix(base, "_bucket") {
			continue
		}
		// Histograms emit suffixed sample lines (_sum, _count);
		// fold them onto the family name contracted here.
		if strings.HasSuffix(base, "_bucket") ||
			strings.HasSuffix(base, "_sum") ||
			strings.HasSuffix(base, "_count") {
			base = base[:strings.LastIndexByte(base, '_')]
		}
		labels, ok := want[base]
		if !ok {
			continue // runtime collectors
		}
		delete(want, base)
		var got []string
		if i := strings.IndexByte(line, '{'); i >= 0 {
			body := line[i+1 : strings.LastIndexByte(line, '}')]
			for _, part := range strings.Split(body, ",") {
				if eq := strings.IndexByte(part, '='); eq > 0 {
					got = append(got, part[:eq])
				}
			}
		} else {
			got = []string{}
		}
		// Exposition output sorts label pairs lexicographically; compare
		// as sets (order-insensitive).
		sort.Strings(got)
		wantSorted := append([]string(nil), labels...)
		sort.Strings(wantSorted)
		if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
			t.Errorf("%s carries labels %v; want %v", name, got, labels)
		}
	}
	for name := range want {
		t.Errorf("metric %s never emitted", name)
	}
}

// TestMetricNamesTopPrefixed pins the namespace convention for all future
// metrics: any collector this package registers must expose top_* names
// (runtime collectors excepted by ecosystem convention).
func TestMetricNamesTopPrefixed(t *testing.T) {
	m, _ := NewMetrics("t", "c")
	out := scrape(t, m)
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue // HELP/TYPE metadata, noise
		}
		name := line
		if i := strings.IndexByte(line, '{'); i >= 0 {
			name = line[:i]
		}
		prefixed := false
		for _, p := range []string{"go_", "process_"} {
			prefixed = prefixed || strings.HasPrefix(name, p)
		}
		if prefixed {
			continue
		}
		if !strings.HasPrefix(name, "top_") {
			t.Errorf("metric outside top_ namespace: %s", line)
		}
	}
}
