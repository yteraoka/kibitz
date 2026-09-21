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
  description = "Model the agent runs, as provider/model. Confirm the provider id against `opencode models` before the first deploy."
  type        = string
  default     = "google-vertex-anthropic/claude-opus-5"
}

variable "vertex_location" {
  description = "Vertex AI location. 'global' has the best availability; pin a region only for data residency, and check that the model is served there."
  type        = string
  default     = "global"
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
