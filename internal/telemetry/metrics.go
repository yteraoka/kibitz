package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the instruments both binaries export. They are deliberately
// few: enough to answer "is kibitz working, how long does it take, and what is
// it costing", which is what an operator asks first.
type Metrics struct {
	registry *prometheus.Registry

	// Server side.
	WebhooksReceived *prometheus.CounterVec
	EventsPublished  *prometheus.CounterVec
	PublishFailures  *prometheus.CounterVec

	// Worker side.
	JobsTotal       *prometheus.CounterVec
	JobDuration     *prometheus.HistogramVec
	JobsInFlight    prometheus.Gauge
	AgentTokens     *prometheus.CounterVec
	CommentsPosted  *prometheus.CounterVec
	FindingsDropped *prometheus.CounterVec
}

// NewMetrics registers the instruments on a fresh registry, so that two
// instances in one process (a test, say) cannot collide.
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: registry,
		WebhooksReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_webhooks_received_total",
			Help: "Webhook deliveries by platform and outcome (published, skipped, rejected).",
		}, []string{"platform", "outcome", "reason"}),
		EventsPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_events_published_total",
			Help: "Normalized events published to the queue.",
		}, []string{"platform", "kind"}),
		PublishFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_publish_failures_total",
			Help: "Events that could not be published. Any value here means reviews were lost.",
		}, []string{"platform"}),
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_jobs_total",
			Help: "Jobs by outcome (succeeded, failed, skipped, abandoned).",
		}, []string{"platform", "kind", "outcome"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kibitz_job_duration_seconds",
			Help: "How long a job took, end to end.",
			// A review runs in minutes, so the buckets reach past the job
			// timeout rather than stopping at the usual ten seconds.
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 900, 1800},
		}, []string{"platform", "kind"}),
		JobsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kibitz_jobs_in_flight",
			Help: "Jobs currently running.",
		}),
		AgentTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_agent_tokens_total",
			Help: "Tokens consumed by the agent, which is what the bill is made of.",
		}, []string{"model", "direction"}),
		CommentsPosted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_comments_posted_total",
			Help: "Findings posted, by severity.",
		}, []string{"platform", "severity"}),
		FindingsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_findings_dropped_total",
			Help: "Findings discarded before posting, by reason.",
		}, []string{"reason"}),
	}

	registry.MustRegister(
		m.WebhooksReceived, m.EventsPublished, m.PublishFailures,
		m.JobsTotal, m.JobDuration, m.JobsInFlight,
		m.AgentTokens, m.CommentsPosted, m.FindingsDropped,
	)
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry, for tests and for anything that needs to
// register its own collectors.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveJob records one finished job.
func (m *Metrics) ObserveJob(platform, kind, outcome string, d time.Duration) {
	m.JobsTotal.WithLabelValues(platform, kind, outcome).Inc()
	m.JobDuration.WithLabelValues(platform, kind).Observe(d.Seconds())
}
