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

variable "server_max_instances" {
  description = <<-EOT
    Upper bound on server instances. The server only verifies a signature and
    publishes, so one instance absorbs a lot of deliveries; the cap is there
    to bound a delivery storm rather than to size for load.

    Raise it if webhooks start being rejected under burst.
  EOT
  type        = number
  default     = 1
}

variable "worker_image" {
  description = "Image for kibitz-worker."
  type        = string
}

variable "scaler_image" {
  description = "Image for kibitz-scaler, which sizes the worker from the queue backlog."
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

variable "mention" {
  description = <<-EOT
    The token that addresses kibitz in a comment.

    GitHub gives it no special meaning: it is matched by kibitz itself, and a
    GitHub App cannot be @-mentioned at all. What GitHub does do is notify the
    account of that name if one exists, and github.com/kibitz is a real
    person, so the "@" form sends them mail from every public repository that
    asks for a review.

    Hence the default slash. It works the same way, commands and arguments
    included, and notifies nobody. Changing it to an "@" form means taking on
    that the name is unclaimed, and stays unclaimed.
  EOT
  type        = string
  default     = "/kibitz"
}

variable "trigger_keywords" {
  description = <<-EOT
    Keywords that ask for a review. When this is empty every pull request is
    reviewed; setting it means only pull requests whose title or description
    contain one of these (or a mention of the bot) are published at all.

    Comments are never gated: addressing the bot is already a request.

    It is also what keeps the queue empty enough for the worker to sit at
    zero instances, so a busy repository wants it set.
  EOT
  type        = list(string)
  default     = []
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

variable "worker_min_instances" {
  description = <<-EOT
    Worker instances kept running when the queue is empty. Zero is the point
    of the autoscaler: between reviews nothing runs and nothing is billed.
    Set 1 to keep the worker warm and still have it scale out under load.
  EOT
  type        = number
  default     = 0
}

variable "worker_idle_after" {
  description = <<-EOT
    How long the queue must be empty before the worker is scaled away. It has
    to outlast Cloud Monitoring's own delay in reporting the backlog, which is
    why the default is generous; shortening it risks removing an instance that
    is still working.
  EOT
  type        = string
  default     = "15m"
}

variable "worker_messages_per_instance" {
  description = "Queued messages one worker instance is expected to absorb before another is added. It normally matches worker_concurrency."
  type        = number
  default     = 2
}

variable "scaler_schedule" {
  description = "How often the scaler reconciles, as a cron expression. Every minute is the finest Cloud Scheduler allows; the server wakes the worker directly, so this only decides how quickly it scales out and back down."
  type        = string
  default     = "* * * * *"
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
