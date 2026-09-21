package opencode

import (
	"context"
	"fmt"
	"strings"
)

// openAICompatibleNPM is the AI SDK package OpenCode loads for a provider that
// speaks the OpenAI chat completions API.
const openAICompatibleNPM = "@ai-sdk/openai-compatible"

// CustomProvider declares a provider OpenCode's own catalog does not know
// about. Vertex AI's Model as a Service partner models are the case this
// exists for: Vertex serves them through an OpenAI-compatible endpoint, but
// models.dev lists only the models OpenCode ships support for, so a model like
// GLM has to be declared by whoever wants to use it.
type CustomProvider struct {
	// ID is the provider half of a model name, e.g. "vertex-maas" in
	// "vertex-maas/zai-org/glm-5.2-maas".
	ID      string
	Name    string
	BaseURL string
	// Token returns a credential for the provider. It is called once per job,
	// which is what makes a short-lived OAuth token workable: the config is
	// written fresh for every run and a job is bounded well inside the
	// token's lifetime.
	Token func(context.Context) (string, error)
}

// Serves reports whether model belongs to this provider.
func (p *CustomProvider) Serves(model string) bool {
	if p == nil || p.ID == "" {
		return false
	}
	provider, _, ok := strings.Cut(model, "/")
	return ok && provider == p.ID
}

// modelID returns the provider-specific half of a model name. Vertex names
// partner models "publisher/model", so the remainder can itself contain a
// slash.
func (p *CustomProvider) modelID(model string) string {
	_, id, _ := strings.Cut(model, "/")
	return id
}

// block renders the provider entry for the generated config.
func (p *CustomProvider) block(ctx context.Context, model string) (map[string]any, error) {
	if p.BaseURL == "" {
		return nil, fmt.Errorf("opencode: no base url for provider %q", p.ID)
	}
	if p.Token == nil {
		return nil, fmt.Errorf("opencode: no credential source for provider %q", p.ID)
	}

	token, err := p.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("opencode: getting a credential for %q: %w", p.ID, err)
	}

	id := p.modelID(model)
	name := p.Name
	if name == "" {
		name = p.ID
	}

	return map[string]any{
		p.ID: map[string]any{
			"npm":  openAICompatibleNPM,
			"name": name,
			"options": map[string]any{
				"baseURL": p.BaseURL,
				"apiKey":  token,
			},
			"models": map[string]any{
				id: map[string]any{"name": id},
			},
		},
	}, nil
}

// VertexMaaSBaseURL builds the OpenAI-compatible endpoint Vertex AI serves its
// partner models on.
//
// The global endpoint has no region prefix on the host, while a regional one
// does; both carry the location in the path.
func VertexMaaSBaseURL(projectID, location string) string {
	if projectID == "" || location == "" {
		return ""
	}
	host := location + "-aiplatform.googleapis.com"
	if location == "global" {
		host = "aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1beta1/projects/%s/locations/%s/endpoints/openapi",
		host, projectID, location)
}
