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

// scalingModeManual is Cloud Run's "run exactly this many instances" mode.
const scalingModeManual = "MANUAL"

// CloudRunService is a Cloud Run service whose instance floor kibitz moves.
//
// It is the *service* level scaling setting that is written, not the one on
// the revision template: the template belongs to the deployment (Terraform,
// gcloud) and changing it would roll a new revision, which would interrupt
// whatever review the current instance is in the middle of.
type CloudRunService struct {
	services *run.ProjectsLocationsServicesService
	name     string
}

// NewCloudRunService addresses one service. It uses the regional endpoint,
// which is the one Cloud Run's v2 API documents for per-service calls.
func NewCloudRunService(ctx context.Context, project, region, service string, opts ...option.ClientOption) (*CloudRunService, error) {
	switch {
	case project == "":
		return nil, fmt.Errorf("project is required")
	case region == "":
		return nil, fmt.Errorf("region is required")
	case service == "":
		return nil, fmt.Errorf("service is required")
	}

	opts = append([]option.ClientOption{
		option.WithEndpoint(fmt.Sprintf("https://%s-run.googleapis.com/", region)),
	}, opts...)

	svc, err := run.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("cloud run client: %w", err)
	}
	return &CloudRunService{
		services: run.NewProjectsService(svc).Locations.Services,
		name:     fmt.Sprintf("projects/%s/locations/%s/services/%s", project, region, service),
	}, nil
}

// String names the service for logs.
func (c *CloudRunService) String() string { return c.name }

// Instances reports the floor the service is currently held at.
func (c *CloudRunService) Instances(ctx context.Context) (int, error) {
	svc, err := c.get(ctx)
	if err != nil {
		return 0, err
	}
	return instancesOf(svc.Scaling), nil
}

// SetInstances writes the floor. A service still on its deployment defaults
// has no scaling block at all, which is why the whole message is sent.
func (c *CloudRunService) SetInstances(ctx context.Context, n int) error {
	if n < 0 {
		return fmt.Errorf("instance count %d is negative", n)
	}

	svc, err := c.get(ctx)
	if err != nil {
		return err
	}

	scaling := &run.GoogleCloudRunV2ServiceScaling{}
	if svc.Scaling != nil {
		*scaling = *svc.Scaling
	}
	// Zero is the value this package exists to write, so it has to be sent
	// explicitly rather than omitted as an empty field.
	if scaling.ScalingMode == scalingModeManual {
		scaling.ManualInstanceCount = int64(n)
		scaling.ForceSendFields = []string{"ManualInstanceCount"}
	} else {
		scaling.MinInstanceCount = int64(n)
		scaling.ForceSendFields = []string{"MinInstanceCount"}
	}

	_, err = c.services.
		Patch(c.name, &run.GoogleCloudRunV2Service{Scaling: scaling}).
		UpdateMask("scaling").
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("patch %s scaling: %w", c.name, describe(err))
	}
	return nil
}

func (c *CloudRunService) get(ctx context.Context) (*run.GoogleCloudRunV2Service, error) {
	svc, err := c.services.Get(c.name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", c.name, describe(err))
	}
	return svc, nil
}

func instancesOf(s *run.GoogleCloudRunV2ServiceScaling) int {
	if s == nil {
		return 0
	}
	if s.ScalingMode == scalingModeManual {
		return int(s.ManualInstanceCount)
	}
	return int(s.MinInstanceCount)
}

// describe turns the Google API client's error into one that says what to fix.
// A 403 here is nearly always a missing role rather than a bug, and the
// message is worth more than the stack it would otherwise be found from.
func describe(err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
		return fmt.Errorf("%w (the caller needs roles/run.viewer to read and roles/run.developer to change the service's scaling)", err)
	}
	return err
}
