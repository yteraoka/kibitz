package scale

import (
	"context"
	"fmt"
	"sort"
	"time"

	monitoring "google.golang.org/api/monitoring/v3"
	"google.golang.org/api/option"
)

// Cloud Monitoring writes one point a minute for a subscription, so that is
// the finest alignment worth asking for.
const alignmentPeriod = time.Minute

// Metric types read from Cloud Monitoring.
const (
	metricUndelivered = "pubsub.googleapis.com/subscription/num_undelivered_messages"
	metricOldestAge   = "pubsub.googleapis.com/subscription/oldest_unacked_message_age"
)

// PubSubBacklog reads a subscription's backlog from Cloud Monitoring.
//
// Pub/Sub itself does not expose the backlog over its API, which is why this
// goes through Monitoring: the numbers are a few minutes behind, and the
// [Policy] that reads them is built around that delay.
type PubSubBacklog struct {
	series       *monitoring.ProjectsTimeSeriesService
	project      string
	subscription string
}

// NewPubSubBacklog watches one subscription.
func NewPubSubBacklog(ctx context.Context, project, subscription string, opts ...option.ClientOption) (*PubSubBacklog, error) {
	switch {
	case project == "":
		return nil, fmt.Errorf("project is required")
	case subscription == "":
		return nil, fmt.Errorf("subscription is required")
	}

	svc, err := monitoring.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("monitoring client: %w", err)
	}
	return &PubSubBacklog{
		series:       monitoring.NewProjectsService(svc).TimeSeries,
		project:      project,
		subscription: subscription,
	}, nil
}

// String names the subscription for logs.
func (p *PubSubBacklog) String() string {
	return fmt.Sprintf("projects/%s/subscriptions/%s", p.project, p.subscription)
}

// Backlog reports the subscription's state over the last window.
func (p *PubSubBacklog) Backlog(ctx context.Context, window time.Duration) (Backlog, error) {
	if window < 2*alignmentPeriod {
		window = 2 * alignmentPeriod
	}
	end := time.Now()
	start := end.Add(-window)

	points, err := p.read(ctx, metricUndelivered, start, end)
	if err != nil {
		return Backlog{}, err
	}
	if len(points) == 0 {
		// A subscription that exists always has points, so an empty answer
		// means the metric has not landed yet (a new subscription, or a
		// Monitoring delay). Reporting it as unknown keeps the worker up.
		return Backlog{}, nil
	}

	backlog := Backlog{
		Known:       true,
		Undelivered: int64(points[0].value),
		EmptyFor:    emptyFor(points),
	}

	// The age of the oldest unacknowledged message is for the operator, so
	// failing to read it does not fail the reading that matters.
	if age, err := p.read(ctx, metricOldestAge, start, end); err == nil && len(age) > 0 {
		backlog.Oldest = time.Duration(age[0].value) * time.Second
	}
	return backlog, nil
}

// sample is one aligned point, newest first.
type sample struct {
	at    time.Time
	value float64
}

func (p *PubSubBacklog) read(ctx context.Context, metric string, start, end time.Time) ([]sample, error) {
	filter := fmt.Sprintf(
		`metric.type=%q AND resource.type="pubsub_subscription" AND resource.labels.subscription_id=%q`,
		metric, p.subscription)

	resp, err := p.series.List("projects/" + p.project).
		Filter(filter).
		IntervalStartTime(start.UTC().Format(time.RFC3339)).
		IntervalEndTime(end.UTC().Format(time.RFC3339)).
		AggregationAlignmentPeriod(fmt.Sprintf("%ds", int(alignmentPeriod.Seconds()))).
		// The maximum over each minute, not the mean: a message that arrived
		// and was handled inside one alignment period still has to count, or
		// the queue would look like it had been empty the whole time.
		AggregationPerSeriesAligner("ALIGN_MAX").
		View("FULL").
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", metric, p, describe(err))
	}

	var samples []sample
	for _, series := range resp.TimeSeries {
		for _, point := range series.Points {
			if point.Interval == nil || point.Value == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, point.Interval.EndTime)
			if err != nil {
				continue
			}
			switch {
			case point.Value.Int64Value != nil:
				samples = append(samples, sample{at: at, value: float64(*point.Value.Int64Value)})
			case point.Value.DoubleValue != nil:
				samples = append(samples, sample{at: at, value: *point.Value.DoubleValue})
			}
		}
	}

	// The API returns points newest first, but a filter can match more than
	// one series (it should not here) and the order is not worth trusting.
	sort.Slice(samples, func(i, j int) bool { return samples[i].at.After(samples[j].at) })
	return samples, nil
}

// emptyFor measures how far back from the newest sample the queue has been
// empty. It is bounded by the window that was read, which is why the caller
// asks for a window longer than the idle threshold it is testing against.
func emptyFor(points []sample) time.Duration {
	if len(points) == 0 || points[0].value > 0 {
		return 0
	}

	newest := points[0].at
	for _, point := range points {
		if point.value > 0 {
			return newest.Sub(point.at)
		}
	}
	// Every point in the window is zero, so the queue has been empty for at
	// least the whole window; the last point's own alignment period counts.
	return newest.Sub(points[len(points)-1].at) + alignmentPeriod
}
