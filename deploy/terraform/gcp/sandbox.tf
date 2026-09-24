# Where a repository's own build and tests run.
#
# Implement mode writes code, and code has to be built before it is worth
# anybody's review. Running that in the worker would mean running a
# repository's test suite in a process holding the GitHub App's private key,
# the GitLab and Azure DevOps tokens, and credentials for Firestore, Pub/Sub
# and Vertex AI. A test file is a place anybody who can open a pull request may
# put code.
#
# So it runs here instead: a Cloud Run job whose service account has no roles
# at all beyond one bucket. A hostile test can reach the metadata server and
# mint a token, and the token opens nothing. Cloud Run already runs containers
# under gVisor, so the host is somebody else's problem, solved.
#
# Everything in this file is created only when implement mode is enabled.
# A deployment that only reviews pull requests pays for none of it.

locals {
  sandbox_enabled = var.implement_enabled
}

# Said at plan time rather than left to a job that starts and cannot pull.
check "runner_image_is_set" {
  assert {
    condition     = !var.implement_enabled || trimspace(var.runner_image) != ""
    error_message = "implement_enabled is true, so runner_image has to name the kibitz-runner image; `make push` prints it."
  }
}

# The one thing the runner can reach. A workspace goes in, a result comes out,
# and a lifecycle rule deletes both: a bucket of other people's source code is
# not something to accumulate.
resource "google_storage_bucket" "sandbox" {
  count = local.sandbox_enabled ? 1 : 0

  name     = "${var.project_id}-${var.name_prefix}-sandbox"
  location = var.region
  labels   = var.labels

  # The trees are written by the worker and read once. Nothing here is worth
  # keeping, and everything in it came from a repository.
  lifecycle_rule {
    condition {
      age = 1
    }
    action {
      type = "Delete"
    }
  }

  uniform_bucket_level_access = true
  # Public access would mean publishing other people's source.
  public_access_prevention = "enforced"

  force_destroy = true

  depends_on = [google_project_service.required]
}

# The runner's identity. It is granted exactly one thing, below, and nothing
# else anywhere: no Vertex AI, no Firestore, no Pub/Sub, no Secret Manager.
# That emptiness is the security property this whole file exists for.
resource "google_service_account" "runner" {
  count = local.sandbox_enabled ? 1 : 0

  account_id   = "${var.name_prefix}-runner"
  display_name = "kibitz sandbox runner (holds no credentials on purpose)"
  description  = "Runs repositories' own build and tests. Granted one bucket and nothing else."
}

resource "google_storage_bucket_iam_member" "runner_sandbox" {
  count = local.sandbox_enabled ? 1 : 0

  bucket = google_storage_bucket.sandbox[0].name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.runner[0].email}"
}

# The worker writes the tree and reads the result.
resource "google_storage_bucket_iam_member" "worker_sandbox" {
  count = local.sandbox_enabled ? 1 : 0

  bucket = google_storage_bucket.sandbox[0].name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.worker.email}"
}

resource "google_cloud_run_v2_job" "runner" {
  count = local.sandbox_enabled ? 1 : 0

  name     = "${var.name_prefix}-runner"
  location = var.region
  labels   = var.labels

  template {
    template {
      service_account = google_service_account.runner[0].email
      # One attempt. A build that failed because of the code fails the same
      # way twice, and a verification kibitz retried would spend twice as long
      # telling somebody the same thing.
      max_retries = 0
      timeout     = var.implement_verify_timeout

      containers {
        image = var.runner_image

        resources {
          limits = {
            cpu    = var.implement_verify_cpu
            memory = var.implement_verify_memory
          }
        }

        # The location is the only thing this job is told, and the worker
        # overrides it per execution. It has no default worth having: a job
        # started without one should fail rather than verify the wrong tree.
        env {
          name  = "KIBITZ_RUNNER_LOCATION"
          value = ""
        }
      }
    }
  }

  lifecycle {
    # The release workflow deploys the image, as it does for the others.
    ignore_changes = [template[0].template[0].containers[0].image]
  }

  depends_on = [google_storage_bucket_iam_member.runner_sandbox]
}

# The worker starts one execution per verification, so it needs to run this
# job -- and only this job.
resource "google_cloud_run_v2_job_iam_member" "worker_runs_runner" {
  count = local.sandbox_enabled ? 1 : 0

  name     = google_cloud_run_v2_job.runner[0].name
  location = google_cloud_run_v2_job.runner[0].location
  role     = "roles/run.developer"
  member   = "serviceAccount:${google_service_account.worker.email}"
}

# Starting an execution runs the job as the runner's service account, which
# the worker may only do if it is allowed to act as it.
resource "google_service_account_iam_member" "worker_acts_as_runner" {
  count = local.sandbox_enabled ? 1 : 0

  service_account_id = google_service_account.runner[0].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.worker.email}"
}
