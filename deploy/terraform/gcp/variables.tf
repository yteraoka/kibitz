variable "project_id" {
  description = "Google Cloud project that hosts kibitz."
  type        = string
}

variable "region" {
  description = "Region for Cloud Run and Artifact Registry."
  type        = string
  default     = "asia-northeast1"
}

variable "firestore_location" {
  description = "Firestore location. It cannot be changed after the database is created."
  type        = string
  default     = "asia-northeast1"
}

variable "name_prefix" {
  description = "Prefix for every resource name, so two environments can share a project."
  type        = string
  default     = "kibitz"
}

variable "server_image" {
  description = "Image for kibitz-server, e.g. REGION-docker.pkg.dev/PROJECT/kibitz/kibitz-server:v1."
  type        = string
}

variable "worker_image" {
  description = "Image for kibitz-worker."
  type        = string
}

variable "model" {
  description = <<-EOT
    Model the agent runs, as provider/model.

    Gemini on Vertex AI is the default because it needs no access request and
    authenticates with the worker's own service account. `google-vertex` serves
    both Gemini and Claude; Claude there has to be requested first, and its ids
    carry a version suffix (google-vertex/claude-opus-5@default).

    Vertex Model Garden's partner models, GLM among them, are reached with
    the vertex-maas prefix (vertex-maas/zai-org/glm-5.2-maas). They also
    authenticate with the service account, so they need no key either;
    kibitz declares them to the agent because its catalog does not list them.

    For a provider that authenticates with an API key, such as GLM through
    Zhipu directly (zai/glm-5.3), set model_api_key_env_name as well.

    `opencode models` inside the worker image lists what a given set of
    credentials can reach.
  EOT
  type        = string
  default     = "google-vertex/gemini-3.1-pro-preview"
}

variable "model_api_key_env_name" {
  description = <<-EOT
    Environment variable the model provider authenticates with, when it needs
    an API key rather than the service account. Leave empty for Vertex AI.

    Examples: ZHIPU_API_KEY for GLM (zai/...), OPENROUTER_API_KEY for
    OpenRouter. Setting this creates a Secret Manager secret to hold the key;
    add the value with `gcloud secrets versions add`.
  EOT
  type        = string
  default     = ""
}

variable "vertex_location" {
  description = "Vertex AI location. 'global' has the best availability; pin a region only for data residency, and check that the model is served there."
  type        = string
  default     = "global"
}

variable "vertex_maas_base_url" {
  description = <<-EOT
    Overrides the OpenAI-compatible endpoint kibitz derives for Vertex Model
    Garden partner models. Leave empty unless the derived URL turns out to be
    wrong; docs/deployment.md has the one-line check.
  EOT
  type        = string
  default     = ""
}

variable "allowed_repos" {
  description = "Repositories kibitz accepts webhooks for, as owner/name wildcards."
  type        = list(string)
  default     = ["*"]
}

variable "bot_logins" {
  description = "Accounts kibitz posts as. Events they author are dropped so the bot never answers itself."
  type        = list(string)
}

variable "github_app_id" {
  description = "GitHub App id."
  type        = string
}

variable "github_installation_id" {
  description = "Installation id of the GitHub App on the organization."
  type        = string
}

variable "worker_concurrency" {
  description = "Reviews one worker instance runs at once. It also bounds how many messages it leases."
  type        = number
  default     = 2
}

variable "worker_max_instances" {
  description = "Upper bound on worker instances. This is the real cap on model spend."
  type        = number
  default     = 3
}

variable "job_timeout" {
  description = "Longest a single review may run."
  type        = string
  default     = "15m"
}

variable "max_delivery_attempts" {
  description = "Deliveries before Pub/Sub moves a message to the dead letter topic. kibitz gives up one attempt earlier and reports the failure on the pull request."
  type        = number
  default     = 5
}

variable "log_level" {
  description = "debug, info, warn or error."
  type        = string
  default     = "info"
}

variable "alert_notification_channels" {
  description = "Monitoring notification channel ids for the alerts. Alerts are created without a channel when this is empty, which means nobody is told."
  type        = list(string)
  default     = []
}

variable "labels" {
  description = "Labels applied to the resources that accept them."
  type        = map(string)
  default     = { app = "kibitz" }
}
