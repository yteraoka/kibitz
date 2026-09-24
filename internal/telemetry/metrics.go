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
	AgentToolCalls  *prometheus.CounterVec
	ReferenceDocs   *prometheus.CounterVec
	CommentsPosted  *prometheus.CounterVec
	FindingsDropped *prometheus.CounterVec
	// ImplementRefused counts instructions the mode that writes code did not
	// act on, by which condition said no. A deployment that has turned the
	// mode on wants to know whether it is refusing for the reason it thinks.
	ImplementRefused *prometheus.CounterVec
	// ImplementVerified counts verifications by whether the change passed. It
	// is the number that says whether the mode is producing changes worth
	// anybody's time: a deployment where nothing ever passes is paying for
	// model calls and getting comments.
	ImplementVerified *prometheus.CounterVec
	// ImplementOpened counts the draft pull requests the mode opened.
	ImplementOpened prometheus.Counter
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
		AgentToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_agent_tool_calls_total",
			Help: "Tool invocations by the agent, by the name the engine reported and whether the call succeeded.",
		}, []string{"tool", "outcome"}),
		ReferenceDocs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_reference_docs_consulted_total",
			Help: "Times the agent went to the repository's decision records: searched across them, or read one in full. Zero over a repository that has them means the index in every prompt is buying nothing.",
		}, []string{"action"}),
		CommentsPosted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_comments_posted_total",
			Help: "Findings posted, by severity.",
		}, []string{"platform", "severity"}),
		FindingsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_findings_dropped_total",
			Help: "Findings discarded before posting, by reason.",
		}, []string{"reason"}),
		ImplementRefused: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_implement_refused_total",
			Help: "Instructions to write code that were refused, by which condition refused them. No repository name: the reason is what an operator acts on.",
		}, []string{"reason"}),
		ImplementVerified: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kibitz_implement_verified_total",
			Help: "Verifications of a written change, by whether it passed.",
		}, []string{"passed"}),
		ImplementOpened: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kibitz_implement_pull_requests_total",
			Help: "Draft pull requests opened by the mode that writes code.",
		}),
	}

	registry.MustRegister(
		m.WebhooksReceived, m.EventsPublished, m.PublishFailures,
		m.JobsTotal, m.JobDuration, m.JobsInFlight,
		m.AgentTokens, m.AgentToolCalls, m.ReferenceDocs,
		m.CommentsPosted, m.FindingsDropped, m.ImplementRefused,
		m.ImplementVerified, m.ImplementOpened,
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
