package opencode_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/reviewer/opencode"
)

func TestCustomProviderServes(t *testing.T) {
	p := &opencode.CustomProvider{ID: "vertex-maas"}

	tests := []struct {
		model string
		want  bool
	}{
		// Vertex names a partner model publisher/model, so the model half
		// contains a slash of its own.
		{"vertex-maas/zai-org/glm-5.2-maas", true},
		{"vertex-maas/anything", true},
		{"google-vertex/gemini-3.1-pro-preview", false},
		{"vertex-maas", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := p.Serves(tc.model); got != tc.want {
			t.Errorf("Serves(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}

	var none *opencode.CustomProvider
	if none.Serves("vertex-maas/x") {
		t.Error("a nil provider claimed a model")
	}
	if (&opencode.CustomProvider{}).Serves("vertex-maas/x") {
		t.Error("a provider with no id claimed a model")
	}
}

func TestVertexMaaSBaseURL(t *testing.T) {
	tests := []struct {
		project, location, want string
	}{
		{
			project:  "my-project",
			location: "asia-northeast1",
			want:     "https://asia-northeast1-aiplatform.googleapis.com/v1beta1/projects/my-project/locations/asia-northeast1/endpoints/openapi",
		},
		{
			// The global endpoint carries no region prefix on the host.
			project:  "my-project",
			location: "global",
			want:     "https://aiplatform.googleapis.com/v1beta1/projects/my-project/locations/global/endpoints/openapi",
		},
		{project: "", location: "global", want: ""},
		{project: "my-project", location: "", want: ""},
	}
	for _, tc := range tests {
		if got := opencode.VertexMaaSBaseURL(tc.project, tc.location); got != tc.want {
			t.Errorf("VertexMaaSBaseURL(%q, %q) =\n%q\nwant\n%q", tc.project, tc.location, got, tc.want)
		}
	}
}

// The credential is minted per job, which is what makes a token that expires
// in an hour workable.
func TestCustomProviderIsWrittenIntoTheConfig(t *testing.T) {
	h := newHarness(t, writeOutput)

	calls := 0
	runner := opencode.New(opencode.Config{
		Bin:   h.bin,
		Model: "vertex-maas/zai-org/glm-5.2-maas",
		CustomProvider: &opencode.CustomProvider{
			ID:      "vertex-maas",
			Name:    "Vertex AI Model Garden",
			BaseURL: "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi",
			Token: func(context.Context) (string, error) {
				calls++
				return "ya29.minted-for-this-job", nil
			},
		},
	}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("the credential was minted %d times, want once per job", calls)
	}

	cfg := h.config(t)
	provider, ok := cfg["provider"].(map[string]any)
	if !ok {
		t.Fatalf("no provider block: %v", cfg)
	}
	entry, ok := provider["vertex-maas"].(map[string]any)
	if !ok {
		t.Fatalf("provider = %v", provider)
	}
	if entry["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %v, want the OpenAI-compatible package", entry["npm"])
	}

	options := entry["options"].(map[string]any)
	if options["apiKey"] != "ya29.minted-for-this-job" {
		t.Errorf("apiKey = %v", options["apiKey"])
	}
	if options["baseURL"] == "" {
		t.Error("baseURL is empty")
	}

	// The declared model keeps its publisher prefix; only the provider half is
	// stripped.
	models := entry["models"].(map[string]any)
	if _, ok := models["zai-org/glm-5.2-maas"]; !ok {
		t.Errorf("models = %v, want the publisher-qualified id", models)
	}
}

// A model the catalog already knows about must not gain a provider block.
func TestNoProviderBlockForCatalogModels(t *testing.T) {
	h := newHarness(t, writeOutput)

	runner := opencode.New(opencode.Config{
		Bin:   h.bin,
		Model: "google-vertex/gemini-3.1-pro-preview",
		CustomProvider: &opencode.CustomProvider{
			ID:      "vertex-maas",
			BaseURL: "https://example.invalid",
			Token:   func(context.Context) (string, error) { return "unused", nil },
		},
	}, discardLogger())

	if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := h.config(t)["provider"]; ok {
		t.Error("a catalog model was given a custom provider block")
	}
}

// Failing to mint a credential fails the job rather than running the agent
// with a config it cannot authenticate.
func TestCredentialFailureStopsTheRun(t *testing.T) {
	h := newHarness(t, writeOutput)

	runner := opencode.New(opencode.Config{
		Bin:   h.bin,
		Model: "vertex-maas/zai-org/glm-5.2-maas",
		CustomProvider: &opencode.CustomProvider{
			ID:      "vertex-maas",
			BaseURL: "https://example.invalid",
			Token:   func(context.Context) (string, error) { return "", errors.New("no application default credentials") },
		},
	}, discardLogger())

	_, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview))
	if err == nil {
		t.Fatal("Run succeeded although no credential could be minted")
	}
	if !errors.Is(err, err) || err.Error() == "" {
		t.Error("the failure carries no explanation")
	}
}

func TestProviderNeedsBaseURLAndToken(t *testing.T) {
	h := newHarness(t, writeOutput)

	for _, tc := range []struct {
		name     string
		provider *opencode.CustomProvider
	}{
		{
			name:     "no base url",
			provider: &opencode.CustomProvider{ID: "vertex-maas", Token: func(context.Context) (string, error) { return "t", nil }},
		},
		{
			name:     "no credential source",
			provider: &opencode.CustomProvider{ID: "vertex-maas", BaseURL: "https://example.invalid"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := opencode.New(opencode.Config{
				Bin:            h.bin,
				Model:          "vertex-maas/zai-org/glm-5.2-maas",
				CustomProvider: tc.provider,
			}, discardLogger())

			if _, err := runner.Run(context.Background(), request(h.workspace, reviewer.ModeReview)); err == nil {
				t.Error("Run succeeded with an incomplete provider")
			}
		})
	}
}
