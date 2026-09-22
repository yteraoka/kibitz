package azuredevops_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
	"github.com/yteraoka/kibitz/internal/webhook/azuredevops"
)

// The fixtures are Microsoft's own published sample payloads, copied verbatim
// from MicrosoftDocs/azure-devops-docs (docs/service-hooks/events.md). They
// are what the shapes here are read from: this platform has three events with
// two different nestings of the same pull request, and getting that from
// memory is how the GitLab handler got two things wrong.
func fixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

const (
	user     = "kibitz"
	password = "s3cret"
)

func handler(t *testing.T, opts ...azuredevops.Option) *azuredevops.Handler {
	t.Helper()

	opts = append([]azuredevops.Option{
		azuredevops.WithClock(func() time.Time { return time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC) }),
	}, opts...)
	return azuredevops.New(user, []string{password}, opts...)
}

func request(body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhook/azure-devops", bytes.NewReader(body))
	r.SetBasicAuth(user, password)
	return r
}

func normalize(t *testing.T, name string) *event.ReviewEvent {
	t.Helper()

	body := fixture(t, name)
	ev, err := handler(t).Normalize(request(body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return ev
}

func TestPullRequestCreated(t *testing.T) {
	ev := normalize(t, "pullrequest-created")
	if ev == nil {
		t.Fatal("a created pull request produced no event")
	}

	if ev.Kind != event.KindPROpened {
		t.Errorf("Kind = %q, want %q", ev.Kind, event.KindPROpened)
	}
	if ev.Source.Platform != event.PlatformAzureDevOps {
		t.Errorf("Platform = %q", ev.Source.Platform)
	}
	// The delivery id is a field of the payload, not a header.
	if ev.Source.DeliveryID != "a0a0a0a0-bbbb-cccc-dddd-e1e1e1e1e1e1" {
		t.Errorf("DeliveryID = %q", ev.Source.DeliveryID)
	}
	if ev.Source.EventName != "git.pullrequest.created" {
		t.Errorf("EventName = %q", ev.Source.EventName)
	}
	if ev.Source.InstanceURL != "https://dev.azure.com/fabrikam" {
		t.Errorf("InstanceURL = %q", ev.Source.InstanceURL)
	}
	if ev.PullRequest.Number != 1 {
		t.Errorf("Number = %d", ev.PullRequest.Number)
	}
	if ev.PullRequest.Source.Branch != "mytopic" || ev.PullRequest.Target.Branch != "main" {
		t.Errorf("branches = %s -> %s", ev.PullRequest.Source.Branch, ev.PullRequest.Target.Branch)
	}
	// The head is the last merge source commit: what the fork's branch is at.
	if ev.PullRequest.Source.SHA != "4444eeee455ff5aaaaabb66ccccccccc7777cccc" {
		t.Errorf("Source.SHA = %q", ev.PullRequest.Source.SHA)
	}
	// uniqueName is the sign-in address, which is stable; displayName is not.
	if ev.Actor.Login != "fabrikamfiber4@hotmail.com" {
		t.Errorf("Actor.Login = %q", ev.Actor.Login)
	}
	if ev.OccurredAt.IsZero() {
		t.Error("OccurredAt is zero")
	}
}

// Azure DevOps nests one level deeper than GitHub and GitLab. The project has
// to be in the name that keys the queue and the lock, or two repositories
// called "api" in one organization become one pull request.
func TestFullNameCarriesTheProject(t *testing.T) {
	ev := normalize(t, "pullrequest-created")

	if got, want := ev.Repository.FullName, "fabrikam/Fabrikam/Fabrikam"; got != want {
		t.Errorf("FullName = %q, want %q", got, want)
	}
	if ev.Repository.Owner != "fabrikam" {
		t.Errorf("Owner = %q", ev.Repository.Owner)
	}
	if ev.Repository.Project != "Fabrikam" {
		t.Errorf("Project = %q", ev.Repository.Project)
	}
	if ev.Repository.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", ev.Repository.DefaultBranch)
	}
	if !bytes.Contains([]byte(ev.Key()), []byte("fabrikam/Fabrikam/Fabrikam")) {
		t.Errorf("Key = %q does not identify the repository fully", ev.Key())
	}
}

// The status is what says a pull request finished, not the event name: the
// sample for git.pullrequest.updated is a completed one.
func TestCompletedPullRequestIsMerged(t *testing.T) {
	ev := normalize(t, "pullrequest-updated")
	if ev == nil {
		t.Fatal("a completed pull request produced no event")
	}

	if ev.Kind != event.KindPRMerged {
		t.Errorf("Kind = %q, want %q", ev.Kind, event.KindPRMerged)
	}
	if !ev.PullRequest.Merged {
		t.Error("PullRequest.Merged is false")
	}
	if ev.PullRequest.State != "completed" {
		t.Errorf("State = %q", ev.PullRequest.State)
	}
}

// An update that is not a completion cannot be told apart from a vote or a
// reviewer change: the payload does not say. It becomes pr.updated, and the
// worker's "already reviewed this commit" check absorbs the rest.
func TestAmbiguousUpdateBecomesAnUpdate(t *testing.T) {
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "pullrequest-updated"), &p); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	resource, _ := p["resource"].(map[string]any)
	resource["status"] = "active"
	delete(resource, "closedDate")
	body, _ := json.Marshal(p)

	ev, err := handler(t).Normalize(request(body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev == nil {
		t.Fatal("an active pull request update produced no event")
	}
	if ev.Kind != event.KindPRUpdated {
		t.Errorf("Kind = %q, want %q", ev.Kind, event.KindPRUpdated)
	}
}

// An abandoned pull request is closed, not merged.
func TestAbandonedPullRequestIsClosed(t *testing.T) {
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "pullrequest-updated"), &p); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	p["resource"].(map[string]any)["status"] = "abandoned"
	body, _ := json.Marshal(p)

	ev, err := handler(t).Normalize(request(body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev == nil || ev.Kind != event.KindPRClosed {
		t.Fatalf("Kind = %v, want %q", ev, event.KindPRClosed)
	}
	if ev.PullRequest.Merged {
		t.Error("an abandoned pull request is reported as merged")
	}
}

// The comment event nests the pull request under "pullRequest" instead of
// putting its fields at the top level, and its resourceContainers carry no
// baseUrl at all. Both are in Microsoft's own samples.
func TestCommentIsNormalized(t *testing.T) {
	body := fixture(t, "pullrequest-commented")
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// The published sample is an edit; make it a new comment.
	c := p["resource"].(map[string]any)["comment"].(map[string]any)
	c["lastUpdatedDate"] = c["publishedDate"]
	c["lastContentUpdatedDate"] = c["publishedDate"]
	body, _ = json.Marshal(p)

	ev, err := handler(t).Normalize(request(body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev == nil {
		t.Fatal("a comment produced no event")
	}

	if ev.Kind != event.KindCommentCreated {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if ev.PullRequest == nil || ev.PullRequest.Number != 1 {
		t.Fatalf("PullRequest = %+v", ev.PullRequest)
	}
	if ev.Comment.Body != "This is my comment." {
		t.Errorf("Comment.Body = %q", ev.Comment.Body)
	}
	// The thread has no field of its own; it is in the thread link.
	if ev.Comment.ThreadID != "5" {
		t.Errorf("ThreadID = %q, want 5", ev.Comment.ThreadID)
	}
	if ev.Comment.InReplyTo != "1" {
		t.Errorf("InReplyTo = %q", ev.Comment.InReplyTo)
	}
	// No baseUrl on this event, so the instance comes from the repository's
	// own URLs — and on visualstudio.com the organization is the subdomain.
	if ev.Source.InstanceURL != "https://fabrikam.visualstudio.com" {
		t.Errorf("InstanceURL = %q", ev.Source.InstanceURL)
	}
	if ev.Repository.Owner != "fabrikam" {
		t.Errorf("Owner = %q, want the subdomain", ev.Repository.Owner)
	}
}

// Azure DevOps sends the same event for writing a comment and for editing
// one. Answering an edit answers the same question twice.
func TestEditedCommentIsIgnored(t *testing.T) {
	ev := normalize(t, "pullrequest-commented")
	if ev != nil {
		t.Errorf("an edited comment produced %q", ev.Kind)
	}
}

// A comment the service wrote itself — a vote, a branch update, a policy
// result — is not somebody asking for anything.
func TestSystemCommentIsIgnored(t *testing.T) {
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "pullrequest-commented"), &p); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	c := p["resource"].(map[string]any)["comment"].(map[string]any)
	c["lastUpdatedDate"] = c["publishedDate"]
	c["commentType"] = "system"
	body, _ := json.Marshal(p)

	ev, err := handler(t).Normalize(request(body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev != nil {
		t.Errorf("a system comment produced %q", ev.Kind)
	}
}

func TestUnsupportedEventIsIgnored(t *testing.T) {
	body := []byte(`{"id":"x","eventType":"git.push","resource":{}}`)
	ev, err := handler(t).Normalize(request(body), body)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev != nil {
		t.Errorf("git.push produced %q", ev.Kind)
	}
}

func TestMalformedPayloadIsTyped(t *testing.T) {
	body := []byte(`{"id":`)
	_, err := handler(t).Normalize(request(body), body)

	var malformed *webhook.ErrMalformedPayload
	if !errors.As(err, &malformed) {
		t.Fatalf("err = %v, want ErrMalformedPayload", err)
	}
}

func TestVerifyAcceptsTheConfiguredCredential(t *testing.T) {
	body := fixture(t, "pullrequest-created")
	if err := handler(t).Verify(request(body), body); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyRejectsAWrongPassword(t *testing.T) {
	body := fixture(t, "pullrequest-created")
	r := httptest.NewRequest(http.MethodPost, "/webhook/azure-devops", bytes.NewReader(body))
	r.SetBasicAuth(user, "wrong")

	err := handler(t).Verify(r, body)
	if !errors.Is(err, webhook.ErrInvalidSignature) {
		t.Errorf("err = %v, want ErrInvalidSignature", err)
	}
}

func TestVerifyRejectsAWrongUser(t *testing.T) {
	body := fixture(t, "pullrequest-created")
	r := httptest.NewRequest(http.MethodPost, "/webhook/azure-devops", bytes.NewReader(body))
	r.SetBasicAuth("someone", password)

	if err := handler(t).Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
		t.Errorf("err = %v, want ErrInvalidSignature", err)
	}
}

// There is no signature to fall back to, so a delivery with no credential at
// all is refused rather than trusted.
func TestVerifyRejectsAnUnauthenticatedDelivery(t *testing.T) {
	body := fixture(t, "pullrequest-created")
	r := httptest.NewRequest(http.MethodPost, "/webhook/azure-devops", bytes.NewReader(body))

	if err := handler(t).Verify(r, body); !errors.Is(err, webhook.ErrMissingSignature) {
		t.Errorf("err = %v, want ErrMissingSignature", err)
	}
}

func TestVerifyWithoutCredentialsIsAMisconfiguration(t *testing.T) {
	body := fixture(t, "pullrequest-created")
	h := azuredevops.New("", nil)

	if err := h.Verify(request(body), body); !errors.Is(err, webhook.ErrNoSecrets) {
		t.Errorf("err = %v, want ErrNoSecrets", err)
	}
}

// The optional header is a second credential, not an alternative: basic auth
// alone does not get in once one is required.
func TestRequiredHeaderIsAlsoChecked(t *testing.T) {
	body := fixture(t, "pullrequest-created")
	h := handler(t, azuredevops.WithHeader("X-Kibitz-Token", []string{"shared", "rotating"}))

	if err := h.Verify(request(body), body); !errors.Is(err, webhook.ErrMissingSignature) {
		t.Errorf("a delivery without the header was accepted: %v", err)
	}

	r := request(body)
	r.Header.Set("X-Kibitz-Token", "nope")
	if err := h.Verify(r, body); !errors.Is(err, webhook.ErrInvalidSignature) {
		t.Errorf("a delivery with a wrong header was accepted: %v", err)
	}

	for _, value := range []string{"shared", "rotating"} {
		r := request(body)
		r.Header.Set("X-Kibitz-Token", value)
		if err := h.Verify(r, body); err != nil {
			t.Errorf("Verify with %q: %v", value, err)
		}
	}
}

// The identifiers are in the payload, so a delivery that verified but was not
// interesting can still be found in Azure DevOps's own log.
func TestDeliveryIdentifiersComeFromTheBody(t *testing.T) {
	var h webhook.BodyDeliveryDescriber = handler(t)

	id, name := h.DeliveryFromBody(fixture(t, "pullrequest-created"))
	if id != "a0a0a0a0-bbbb-cccc-dddd-e1e1e1e1e1e1" {
		t.Errorf("id = %q", id)
	}
	if name != "git.pullrequest.created" {
		t.Errorf("name = %q", name)
	}

	if id, name := h.DeliveryFromBody([]byte("not json")); id != "" || name != "" {
		t.Errorf("a malformed body reported %q/%q", id, name)
	}
}
