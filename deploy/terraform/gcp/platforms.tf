# GitLab and Azure DevOps.
#
# Both are off by default, because a deployment that reviews only GitHub is
# the common case and a secret nobody fills in is a secret somebody has to
# explain later. Turning one on creates its secrets and grants the two
# service accounts what each needs: the server verifies deliveries, the
# worker calls the API, and neither reads the other's credential.
#
# As everywhere else here, the values are added with `gcloud secrets versions
# add` rather than by Terraform, so they never enter the state file.

# --- GitLab -------------------------------------------------------------

# The shared token says who sent a delivery. The signing token covers the
# body as well, so it is the one to use where the instance is new enough;
# both exist so an instance can be migrated without a gap.
resource "google_secret_manager_secret" "gitlab_webhook_tokens" {
  count = var.gitlab_enabled ? 1 : 0

  secret_id = "${var.name_prefix}-gitlab-webhook-tokens"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "gitlab_signing_tokens" {
  count = var.gitlab_enabled ? 1 : 0

  secret_id = "${var.name_prefix}-gitlab-signing-tokens"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

# GitLab has no installation token, so this one is long lived. A group access
# token limits what it reaches; rotating it is in docs/runbook.md.
resource "google_secret_manager_secret" "gitlab_token" {
  count = var.gitlab_enabled ? 1 : 0

  secret_id = "${var.name_prefix}-gitlab-token"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret_iam_member" "server_gitlab_webhook_tokens" {
  count = var.gitlab_enabled ? 1 : 0

  secret_id = google_secret_manager_secret.gitlab_webhook_tokens[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}

resource "google_secret_manager_secret_iam_member" "server_gitlab_signing_tokens" {
  count = var.gitlab_enabled ? 1 : 0

  secret_id = google_secret_manager_secret.gitlab_signing_tokens[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}

resource "google_secret_manager_secret_iam_member" "worker_gitlab_token" {
  count = var.gitlab_enabled ? 1 : 0

  secret_id = google_secret_manager_secret.gitlab_token[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.worker.email}"
}

# --- Azure DevOps -------------------------------------------------------

# Azure DevOps does not sign its service hooks, so basic authentication is
# the whole credential and this secret is the only thing between the queue
# and anyone who can reach the URL.
resource "google_secret_manager_secret" "azure_devops_passwords" {
  count = var.azure_devops_enabled ? 1 : 0

  secret_id = "${var.name_prefix}-azdo-basic-passwords"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

# The optional second credential. It exists so that a leaked password on its
# own does not let somebody forge a pull request event.
resource "google_secret_manager_secret" "azure_devops_header_values" {
  count = var.azure_devops_enabled && var.azure_devops_header_name != "" ? 1 : 0

  secret_id = "${var.name_prefix}-azdo-header-values"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret" "azure_devops_token" {
  count = var.azure_devops_enabled ? 1 : 0

  secret_id = "${var.name_prefix}-azdo-token"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret_iam_member" "server_azure_devops_passwords" {
  count = var.azure_devops_enabled ? 1 : 0

  secret_id = google_secret_manager_secret.azure_devops_passwords[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}

resource "google_secret_manager_secret_iam_member" "server_azure_devops_header_values" {
  count = var.azure_devops_enabled && var.azure_devops_header_name != "" ? 1 : 0

  secret_id = google_secret_manager_secret.azure_devops_header_values[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}

resource "google_secret_manager_secret_iam_member" "worker_azure_devops_token" {
  count = var.azure_devops_enabled ? 1 : 0

  secret_id = google_secret_manager_secret.azure_devops_token[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.worker.email}"
}
