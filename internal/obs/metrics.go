package obs

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric label enumerations. Every label value on TOP's metrics comes from
// one of these sets or from operator-bounded inputs (service refs) — never
// from unbounded data like git shas, error strings, URLs, delivery ids or
// repo URLs (ADR-0010 cardinality discipline).
const (
	// ResultOK / ResultDrift / ResultError are the values of every
	// `result` label.
	ResultOK    = "ok"
	ResultDrift = "drift"
	ResultError = "error"

	// LabelService is a service ref "ns/name"; the set is bounded by the
	// operator's repos.
	LabelService = "service"

	// top_ prefix for every metric.
	namespace = "top"
)

// Metrics bundles the ADR-0010 metric set against a private registry
// (never prometheus.DefaultRegisterer — TOP must not fight the host
// process's default metrics or enforced duplicate registration).
type Metrics struct {
	// Registry is exposed so the controller server can serve its own
	// handler without obs owning a listener.
	Registry *prometheus.Registry

	reconcileDuration   prometheus.Histogram
	reconcileTotal      *prometheus.CounterVec
	serviceDrift        *prometheus.GaugeVec
	deployTotal         *prometheus.CounterVec
	deployDuration      *prometheus.HistogramVec
	lastSuccessReconcil prometheus.Gauge
	webhookDeliveries   *prometheus.CounterVec
	gitPollDuration     prometheus.Histogram
	gitPollTotal        *prometheus.CounterVec
	sqliteOpDuration    *prometheus.HistogramVec
	backupLastSuccess   prometheus.Gauge
	backupVerifyResult  prometheus.Gauge
	backupDuration      prometheus.Histogram
	backupTotal         *prometheus.CounterVec
	buildInfo           prometheus.Gauge
}

// NewMetrics builds the full ADR-0010 set, registering every collector and
// the Go/process collectors of this process. buildInfo is set to the given
// version/commit exactly once here; re-calling with different values on the
// same instance is not supported (a new process deploy creates a new Metrics).
func NewMetrics(version, commit string) (*Metrics, error) {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of whole reconcile runs (fetch→apply→record).",
			// Whole-run duration, known to be slow-ish; 1s..10m buckets.
			Buckets: prometheus.ExponentialBucketsRange(0.1, 600, 12),
		}),
		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_total",
			Help:      "Reconcile runs by outcome.",
		}, []string{"result"}),
		serviceDrift: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "service_drift",
			Help:      "1 when a service's observed state differs from desired, 0 when in sync.",
		}, []string{LabelService}),
		deployTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "deploy_total",
			Help:      "Per-service deploys by outcome.",
		}, []string{LabelService, "result"}),
		deployDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "deploy_duration_seconds",
			Help:      "Duration of per-service deploys (apply + readiness gate).",
			Buckets:   prometheus.ExponentialBucketsRange(0.1, 300, 10),
		}, []string{LabelService}),
		lastSuccessReconcil: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_successful_reconcile_timestamp",
			Help:      "Unix timestamp of the last fully successful reconcile run.",
		}),
		webhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "webhook_deliveries_total",
			Help:      "Received webhook deliveries by event type and processing outcome.",
		}, []string{"event", "result"}),
		gitPollDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "git_poll_duration_seconds",
			Help:      "Duration of one poll cycle against the Git source.",
			Buckets:   prometheus.ExponentialBucketsRange(0.1, 300, 10),
		}),
		gitPollTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "git_poll_total",
			Help:      "Git poll cycles by outcome.",
		}, []string{"result"}),
		sqliteOpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "sqlite_operation_duration_seconds",
			Help:      "Latency of state-store operations by operation kind.",
			Buckets:   prometheus.ExponentialBucketsRange(0.001, 10, 10),
		}, []string{"operation"}),
		backupLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "backup_last_success_timestamp",
			Help:      "Unix timestamp of the last successful backup.",
		}),
		backupVerifyResult: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "backup_last_verify_result",
			Help:      "Outcome of the last backup verification: 1 ok, 0 failed.",
		}),
		backupDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "backup_duration_seconds",
			Help:      "Duration of one backup Create (VACUUM INTO → fsync → registry).",
			Buckets:   prometheus.ExponentialBucketsRange(0.01, 300, 10),
		}),
		backupTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "backup_total",
			Help:      "Backup Create attempts by outcome.",
		}, []string{"result"}),
		buildInfo: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Build metadata of the running controller; always 1.",
			ConstLabels: prometheus.Labels{
				"version": version,
				"commit":  commit,
			},
		}),
	}

	register := func(c prometheus.Collector) error { return m.Registry.Register(c) }
	// Go and process collectors: both are safe cross-platform (process
	// metrics simply go empty when /proc-style info is unavailable, as on
	// darwin dev hosts). A duplicate registration here is a programming
	// error in this function's metric list, so it short-circuits New.
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.reconcileDuration,
		m.reconcileTotal,
		m.serviceDrift,
		m.deployTotal,
		m.deployDuration,
		m.lastSuccessReconcil,
		m.webhookDeliveries,
		m.gitPollDuration,
		m.gitPollTotal,
		m.sqliteOpDuration,
		m.backupLastSuccess,
		m.backupVerifyResult,
		m.backupDuration,
		m.backupTotal,
		m.buildInfo,
	} {
		if err := register(c); err != nil {
			return nil, err
		}
	}
	m.buildInfo.Set(1)
	return m, nil
}

// --- Instrumentation surface --------------------------------------------
// Friendly setter/observe helpers so call sites never touch label maps.

// RecordReconcile records one whole-run outcome with its duration.
func (m *Metrics) RecordReconcile(d time.Duration, result string) {
	m.reconcileDuration.Observe(d.Seconds())
	m.reconcileTotal.WithLabelValues(result).Inc()
}

// SetServiceDrift marks a service drifted (1) or in sync (0).
func (m *Metrics) SetServiceDrift(service string, drift bool) {
	v := 0.0
	if drift {
		v = 1
	}
	m.serviceDrift.WithLabelValues(service).Set(v)
}

// RecordDeploy records one per-service deploy outcome with its duration.
func (m *Metrics) RecordDeploy(service string, d time.Duration, result string) {
	m.deployTotal.WithLabelValues(service, result).Inc()
	m.deployDuration.WithLabelValues(service).Observe(d.Seconds())
}

// SetLastSuccessfulReconcileAt stamps a timestamp (Unix seconds) — call with
// time.Now() after a fully green run.
func (m *Metrics) SetLastSuccessfulReconcileAt(t time.Time) {
	m.lastSuccessReconcil.Set(float64(t.Unix()))
}

// RecordWebhookDelivery counts one delivery's processing outcome. event is a
// bounded set defined by the webhook contract (not raw payloads).
func (m *Metrics) RecordWebhookDelivery(event, result string) {
	m.webhookDeliveries.WithLabelValues(event, result).Inc()
}

// RecordGitPoll records one poll cycle's outcome with its duration.
func (m *Metrics) RecordGitPoll(d time.Duration, result string) {
	m.gitPollDuration.Observe(d.Seconds())
	m.gitPollTotal.WithLabelValues(result).Inc()
}

// ObserveSQLiteOperation records one state-store operation's latency.
// operation is a bounded set (e.g. "get", "put", "lease", "delete") defined
// by the StateStore contract — never per-record ids.
func (m *Metrics) ObserveSQLiteOperation(operation string, d time.Duration) {
	m.sqliteOpDuration.WithLabelValues(operation).Observe(d.Seconds())
}

// SetBackupSuccessAt stamps the last successful backup timestamp.
func (m *Metrics) SetBackupSuccessAt(t time.Time) {
	m.backupLastSuccess.Set(float64(t.Unix()))
}

// SetBackupVerifyResult records the last verification outcome: ok → 1.
func (m *Metrics) SetBackupVerifyResult(ok bool) {
	v := 0.0
	if ok {
		v = 1
	}
	m.backupVerifyResult.Set(v)
}

// RecordBackup records one backup attempt's outcome with its duration.
// result is the bounded `result` set ("ok"/"error"), never an error string
// (ADR-0010 cardinality discipline).
func (m *Metrics) RecordBackup(d time.Duration, result string) {
	m.backupDuration.Observe(d.Seconds())
	m.backupTotal.WithLabelValues(result).Inc()
}

// --- Serving -------------------------------------------------------------
// obs never owns a listener (ADR-0006): the controller registers these
// handlers on its own server.

// HTTPHandler returns the /metrics handler over the private registry.
func (m *Metrics) HTTPHandler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ServeMetrics registers /metrics and /healthz on the given mux. /healthz
// returns a trivially cheap 200 — liveness only, readiness is the
// controller's business.
func (m *Metrics) ServeMetrics(mux *http.ServeMux) {
	mux.Handle("/metrics", m.HTTPHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}
