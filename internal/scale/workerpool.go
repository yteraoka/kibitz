package scale

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v2"
)

// CloudRunWorkerPool is a Cloud Run worker pool whose instance count kibitz
// writes.
//
// A worker pool is the shape this workload actually has: no ingress, no
// requests, no request-driven autoscaling, and the CPU always allocated. What
// it offers instead is a manual instance count, which is exactly the number
// this package computes from the backlog.
//
// It is the *pool* level scaling setting that is written, not the one on the
// revision template: the template belongs to the deployment (Terraform,
// gcloud) and changing it would roll a new revision, which would interrupt
// whatever review the current instance is in the middle of.
type CloudRunWorkerPool struct {
	pools *run.ProjectsLocationsWorkerPoolsService
	name  string
}

// NewCloudRunWorkerPool addresses one worker pool. It uses the regional
// endpoint, which is the one Cloud Run's v2 API documents for per-resource
// calls.
func NewCloudRunWorkerPool(ctx context.Context, project, region, pool string, opts ...option.ClientOption) (*CloudRunWorkerPool, error) {
	switch {
	case project == "":
		return nil, fmt.Errorf("project is required")
	case region == "":
		return nil, fmt.Errorf("region is required")
	case pool == "":
		return nil, fmt.Errorf("worker pool is required")
	}

	opts = append([]option.ClientOption{
		option.WithEndpoint(fmt.Sprintf("https://%s-run.googleapis.com/", region)),
	}, opts...)

	svc, err := run.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("cloud run client: %w", err)
	}
	return &CloudRunWorkerPool{
		pools: run.NewProjectsService(svc).Locations.WorkerPools,
		name:  fmt.Sprintf("projects/%s/locations/%s/workerPools/%s", project, region, pool),
	}, nil
}

// String names the worker pool for logs.
func (c *CloudRunWorkerPool) String() string { return c.name }

// Instances reports the count the pool is currently held at.
func (c *CloudRunWorkerPool) Instances(ctx context.Context) (int, error) {
	pool, err := c.get(ctx)
	if err != nil {
		return 0, err
	}
	return instancesOf(pool.Scaling), nil
}

// SetInstances writes the count. A pool still on its deployment defaults has
// no scaling block at all, which is why the whole message is sent.
func (c *CloudRunWorkerPool) SetInstances(ctx context.Context, n int) error {
	if n < 0 {
		return fmt.Errorf("instance count %d is negative", n)
	}

	pool, err := c.get(ctx)
	if err != nil {
		return err
	}

	scaling := &run.GoogleCloudRunV2WorkerPoolScaling{}
	if pool.Scaling != nil {
		*scaling = *pool.Scaling
	}
	// Zero is the value this package exists to write, so it has to be sent
	// explicitly rather than omitted as an empty field.
	scaling.ManualInstanceCount = int64(n)
	scaling.ForceSendFields = []string{"ManualInstanceCount"}

	_, err = c.pools.
		Patch(c.name, &run.GoogleCloudRunV2WorkerPool{Scaling: scaling}).
		UpdateMask("scaling").
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("patch %s scaling: %w", c.name, describe(err))
	}
	return nil
}

func (c *CloudRunWorkerPool) get(ctx context.Context) (*run.GoogleCloudRunV2WorkerPool, error) {
	pool, err := c.pools.Get(c.name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", c.name, describe(err))
	}
	return pool, nil
}

func instancesOf(s *run.GoogleCloudRunV2WorkerPoolScaling) int {
	if s == nil {
		return 0
	}
	return int(s.ManualInstanceCount)
}

// describe turns the Google API client's error into one that says what to fix.
// A 403 here is nearly always a missing role rather than a bug, and the
// message is worth more than the stack it would otherwise be found from.
func describe(err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
		return fmt.Errorf("%w (the caller needs roles/run.viewer to read and roles/run.developer to change the worker pool's scaling)", err)
	}
	return err
}
