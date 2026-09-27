# Secret values are not in Terraform: they are added with `gcloud secrets
# versions add` (see docs/deployment.md), so they never enter the state file.

resource "google_secret_manager_secret" "github_webhook" {
  secret_id = "${var.name_prefix}-github-webhook-secrets"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "github_private_key" {
  secret_id = "${var.name_prefix}-github-app-private-key"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

# Only created when the model provider needs an API key. On Vertex AI there is
# none: the worker authenticates as itself.
resource "google_secret_manager_secret" "model_api_key" {
  count = var.model_api_key_env_name != "" ? 1 : 0

  secret_id = "${var.name_prefix}-model-api-key"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret_iam_member" "worker_model_api_key" {
  count = var.model_api_key_env_name != "" ? 1 : 0

  secret_id = google_secret_manager_secret.model_api_key[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.worker.email}"
}

# The server verifies webhooks, so it reads the webhook secrets; the worker
# calls the API, so it reads the App key. The worker never reads the webhook
# secrets. The server reads the App key only to react to comments, and only
# with server_reactions on (ADR-0020).
resource "google_secret_manager_secret_iam_member" "server_webhook" {
  secret_id = google_secret_manager_secret.github_webhook.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}

resource "google_secret_manager_secret_iam_member" "worker_private_key" {
  secret_id = google_secret_manager_secret.github_private_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.worker.email}"
}

resource "google_secret_manager_secret_iam_member" "server_private_key" {
  count = var.server_reactions ? 1 : 0

  secret_id = google_secret_manager_secret.github_private_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}
