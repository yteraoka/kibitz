package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/telemetry"
)

func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMetricsExposition(t *testing.T) {
	m := telemetry.NewMetrics()

	m.WebhooksReceived.WithLabelValues("github", "published", "").Inc()
	m.EventsPublished.WithLabelValues("github", "pr.opened").Inc()
	m.ObserveJob("github", "pr.opened", "succeeded", 42*time.Second)
	m.CommentsPosted.WithLabelValues("github", "high").Inc()
	m.FindingsDropped.WithLabelValues("out_of_diff").Add(2)
	m.AgentTokens.WithLabelValues("claude-opus-5", "input").Add(1200)
	m.AgentToolCalls.WithLabelValues("kibitz_get_doc", "ok").Add(3)
	m.ReferenceDocs.WithLabelValues("read").Add(2)

	body := scrape(t, m)
	for _, want := range []string{
		`kibitz_webhooks_received_total{outcome="published",platform="github",reason=""} 1`,
		`kibitz_events_published_total{kind="pr.opened",platform="github"} 1`,
		`kibitz_jobs_total{kind="pr.opened",outcome="succeeded",platform="github"} 1`,
		`kibitz_job_duration_seconds_sum{kind="pr.opened",platform="github"} 42`,
		`kibitz_comments_posted_total{platform="github",severity="high"} 1`,
		`kibitz_findings_dropped_total{reason="out_of_diff"} 2`,
		`kibitz_agent_tokens_total{direction="input",model="claude-opus-5"} 1200`,
		`kibitz_agent_tool_calls_total{outcome="ok",tool="kibitz_get_doc"} 3`,
		`kibitz_reference_docs_consulted_total{action="read"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not contain:\n%s", want)
		}
	}
}

// A review runs for minutes, so the histogram has to have buckets up there or
// every job lands in +Inf and the latency graph says nothing.
func TestJobDurationBucketsCoverARealReview(t *testing.T) {
	m := telemetry.NewMetrics()
	m.ObserveJob("github", "pr.opened", "succeeded", 4*time.Minute)

	body := scrape(t, m)
	if !strings.Contains(body, `le="300"`) {
		t.Error("there is no bucket at five minutes")
	}
	if strings.Contains(body, `kibitz_job_duration_seconds_bucket{kind="pr.opened",platform="github",le="300"} 0`) {
		t.Error("a four-minute job did not land in the five-minute bucket")
	}
}

// Two instances in one process must not fight over a global registry.
func TestRegistriesAreIndependent(t *testing.T) {
	a, b := telemetry.NewMetrics(), telemetry.NewMetrics()

	a.PublishFailures.WithLabelValues("github").Inc()
	if strings.Contains(scrape(t, b), "kibitz_publish_failures_total{") {
		t.Error("the second registry saw the first one's counter")
	}
}
