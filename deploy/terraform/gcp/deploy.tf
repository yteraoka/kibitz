# Deploying from GitHub Actions.
#
# The workflow authenticates with Workload Identity Federation: GitHub mints a
# short-lived OIDC token, Google exchanges it for one of this service account's,
# and no key ever exists to leak. A service account key in a repository secret
# is the thing this avoids.
#
# What may use it is narrow on purpose: this repository, and only on a tag. A
# workflow on a branch — including one a pull request introduced — cannot
# exchange a token at all, so it cannot deploy.

# The pool already exists in the project and is shared with whatever else
# federates into it, so Terraform reads it rather than owning it. Only the
# provider below belongs to kibitz, and its condition is what narrows who may
# exchange a token.
data "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = var.workload_identity_pool_id
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = data.google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "${var.name_prefix}-github"
  display_name                       = "GitHub OIDC"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
  }

  # Without a condition, any repository on GitHub could exchange a token.
  attribute_condition = format(
    "assertion.repository == %q && assertion.ref.startsWith(\"refs/tags/\")",
    var.github_repository,
  )

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }

  depends_on = [google_project_service.required]
}

resource "google_service_account" "deployer" {
  account_id   = "${var.name_prefix}-deployer"
  display_name = "kibitz release pipeline"
}

resource "google_service_account_iam_member" "deployer_federated" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member = format(
    "principalSet://iam.googleapis.com/%s/attribute.repository/%s",
    data.google_iam_workload_identity_pool.github.name,
    var.github_repository,
  )
}

# Pushing images. Writer, not admin: the pipeline adds versions and never
# deletes one.
resource "google_artifact_registry_repository_iam_member" "deployer_push" {
  location   = google_artifact_registry_repository.images.location
  repository = google_artifact_registry_repository.images.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.deployer.email}"
}

# Changing which image each service runs. The grants are per service rather
# than on the project, so the pipeline cannot touch anything else here.
resource "google_cloud_run_v2_service_iam_member" "deployer_services" {
  for_each = {
    server = google_cloud_run_v2_service.server.name
    worker = google_cloud_run_v2_service.worker.name
  }

  name     = each.value
  location = var.region
  role     = "roles/run.developer"
  member   = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_cloud_run_v2_job_iam_member" "deployer_scaler" {
  name     = google_cloud_run_v2_job.scaler.name
  location = var.region
  role     = "roles/run.developer"
  member   = "serviceAccount:${google_service_account.deployer.email}"
}

# Deploying a revision means asserting the identity it runs as.
resource "google_service_account_iam_member" "deployer_act_as" {
  for_each = {
    server = google_service_account.server.name
    worker = google_service_account.worker.name
    scaler = google_service_account.scaler.name
  }

  service_account_id = each.value
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.deployer.email}"
}
