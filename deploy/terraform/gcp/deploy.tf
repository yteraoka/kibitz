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

# Changing which image each component runs. The grants are per resource rather
# than on the project, so the pipeline cannot touch anything else here. The
# server is a service and the worker is a worker pool, which are separate
# resource types with separate IAM policies.
resource "google_cloud_run_v2_service_iam_member" "deployer_server" {
  name     = google_cloud_run_v2_service.server.name
  location = var.region
  role     = "roles/run.developer"
  member   = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_cloud_run_v2_worker_pool_iam_member" "deployer_worker" {
  name     = google_cloud_run_v2_worker_pool.worker.name
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

# Watching the deployment finish.
#
# A worker pool only exists in the Cloud Run v2 API, so updating one returns a
# long-running operation and gcloud polls it until the revision is ready. That
# operation is not a child of the worker pool: it lives at
# projects/PROJECT/locations/REGION/operations/ID, where the per-resource
# grants above do not reach, and the poll fails with
#
#   Permission 'run.operations.get' denied
#
# after the update itself has already been accepted. Services and jobs do not
# hit this because gcloud still deploys those through the v1 API, which reports
# progress on the resource instead.
#
# roles/run.viewer would cover it, but it also reads every other Cloud Run
# resource in the project. This is the one permission the poll needs.
resource "google_project_iam_custom_role" "run_operation_reader" {
  role_id     = "${replace(var.name_prefix, "-", "_")}_run_operation_reader"
  title       = "kibitz release pipeline: read Cloud Run operations"
  description = "Poll the long-running operation a worker pool update returns."
  permissions = ["run.operations.get"]
}

resource "google_project_iam_member" "deployer_operations" {
  project = var.project_id
  role    = google_project_iam_custom_role.run_operation_reader.id
  member  = "serviceAccount:${google_service_account.deployer.email}"
}
