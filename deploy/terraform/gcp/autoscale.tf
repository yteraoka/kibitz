# Sizing the worker from the queue.
#
# A worker pool has no request-driven autoscaling: its instance count is a
# number someone states. Left at a constant the worker is either always
# running (paid for around the clock, mostly idle) or never started, so kibitz
# states it from the two ends:
#
#   * kibitz-server sets the count to one the moment it publishes, so a
#     review starts immediately rather than waiting for a metric;
#   * kibitz-scaler, below, reconciles once a minute from the subscription's
#     backlog: it adds instances while messages pile up, and returns the pool
#     to worker_min_instances once the queue has been empty for
#     worker_idle_after.
#
# The backlog counts unacknowledged messages as well as undelivered ones, so a
# worker in the middle of a review still holds its message and is not scaled
# away underneath it.

resource "google_service_account" "scaler" {
  account_id   = "${var.name_prefix}-scaler"
  display_name = "kibitz worker autoscaler"
}

resource "google_cloud_run_v2_job" "scaler" {
  name     = "${var.name_prefix}-scaler"
  location = var.region
  labels   = var.labels

  deletion_protection = false

  template {
    template {
      service_account = google_service_account.scaler.email
      # One reconciliation is one read and at most one write. If it cannot be
      # done in two minutes the next run will do it instead.
      timeout     = "120s"
      max_retries = 1

      containers {
        image = var.scaler_image

        resources {
          limits = {
            cpu    = "1"
            memory = "512Mi"
          }
        }

        env {
          name  = "KIBITZ_PUBSUB_PROJECT_ID"
          value = var.project_id
        }
        env {
          name  = "KIBITZ_PUBSUB_SUBSCRIPTION"
          value = google_pubsub_subscription.worker.name
        }
        env {
          name  = "KIBITZ_SCALE_BACKEND"
          value = "cloudrun"
        }
        env {
          name  = "KIBITZ_SCALE_REGION"
          value = var.region
        }
        env {
          name  = "KIBITZ_SCALE_WORKER_POOL"
          value = google_cloud_run_v2_worker_pool.worker.name
        }
        env {
          name  = "KIBITZ_SCALE_MIN_INSTANCES"
          value = tostring(var.worker_min_instances)
        }
        env {
          name  = "KIBITZ_SCALE_MAX_INSTANCES"
          value = tostring(var.worker_max_instances)
        }
        env {
          name  = "KIBITZ_SCALE_MESSAGES_PER_INSTANCE"
          value = tostring(var.worker_messages_per_instance)
        }
        env {
          name  = "KIBITZ_SCALE_IDLE_AFTER"
          value = var.worker_idle_after
        }
        env {
          name  = "KIBITZ_LOG_LEVEL"
          value = var.log_level
        }
      }
    }
  }

  lifecycle {
    ignore_changes = [template[0].template[0].containers[0].image]
  }

  depends_on = [
    google_project_iam_member.scaler_monitoring,
    google_cloud_run_v2_worker_pool_iam_member.worker_scaling,
  ]
}

# Cloud Scheduler is what makes the job run; nothing else triggers it.
resource "google_cloud_scheduler_job" "scaler" {
  name             = "${var.name_prefix}-scaler"
  region           = var.region
  description      = "Sizes the kibitz worker from the Pub/Sub backlog."
  schedule         = var.scaler_schedule
  time_zone        = "Etc/UTC"
  attempt_deadline = "180s"

  retry_config {
    # A missed run is corrected by the next one a minute later, so there is
    # little point retrying this one.
    retry_count = 1
  }

  http_target {
    http_method = "POST"
    uri         = "https://${var.region}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${var.project_id}/jobs/${google_cloud_run_v2_job.scaler.name}:run"

    oauth_token {
      service_account_email = google_service_account.scaler.email
    }
  }

  depends_on = [
    google_project_service.required,
    google_cloud_run_v2_job_iam_member.scaler_invoke,
  ]
}

# The backlog lives in Cloud Monitoring; Pub/Sub does not report it directly.
resource "google_project_iam_member" "scaler_monitoring" {
  project = var.project_id
  role    = "roles/monitoring.viewer"
  member  = "serviceAccount:${google_service_account.scaler.email}"
}

# Both the scaler and the server change the worker's instance count, and
# nothing else about it. The grant is on the one worker pool rather than the
# project, so neither can touch anything else that runs here.
resource "google_cloud_run_v2_worker_pool_iam_member" "worker_scaling" {
  for_each = {
    scaler = google_service_account.scaler.email
    server = google_service_account.server.email
  }

  name     = google_cloud_run_v2_worker_pool.worker.name
  location = google_cloud_run_v2_worker_pool.worker.location
  role     = "roles/run.developer"
  member   = "serviceAccount:${each.value}"
}

# Updating a Cloud Run resource means asserting the identity it runs as, even
# when the update only changes a number.
resource "google_service_account_iam_member" "worker_act_as" {
  for_each = {
    scaler = google_service_account.scaler.email
    server = google_service_account.server.email
  }

  service_account_id = google_service_account.worker.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${each.value}"
}

# Cloud Scheduler authenticates as the scaler's own account, which therefore
# has to be allowed to start the job.
resource "google_cloud_run_v2_job_iam_member" "scaler_invoke" {
  name     = google_cloud_run_v2_job.scaler.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scaler.email}"
}
