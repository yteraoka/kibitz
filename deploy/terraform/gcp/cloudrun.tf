# The server is on the public internet because GitHub has to reach it. It is
# stateless and does no work beyond verifying and publishing, so it scales to
# zero between deliveries.
resource "google_cloud_run_v2_service" "server" {
  name     = "${var.name_prefix}-server"
  location = var.region
  labels   = var.labels

  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = false

  template {
    service_account = google_service_account.server.email

    scaling {
      min_instance_count = 0
      max_instance_count = 10
    }

    containers {
      image = var.server_image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
        # A webhook that waits for a cold start risks the forge's timeout.
        startup_cpu_boost = true
      }

      env {
        name  = "KIBITZ_QUEUE_BACKEND"
        value = "pubsub"
      }
      env {
        name  = "KIBITZ_PUBSUB_PROJECT_ID"
        value = var.project_id
      }
      env {
        name  = "KIBITZ_PUBSUB_TOPIC"
        value = google_pubsub_topic.events.name
      }
      env {
        name  = "KIBITZ_LOG_LEVEL"
        value = var.log_level
      }
      env {
        name  = "KIBITZ_ALLOWED_REPOS"
        value = join(",", var.allowed_repos)
      }
      env {
        name  = "KIBITZ_BOT_LOGINS"
        value = join(",", var.bot_logins)
      }
      # Cloud Run exposes a single port, so /metrics rides on the main
      # listener. The counters carry no repository or user names.
      env {
        name  = "KIBITZ_METRICS_ADDR"
        value = "off"
      }
      env {
        name = "KIBITZ_GITHUB_WEBHOOK_SECRETS"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.github_webhook.secret_id
            version = "latest"
          }
        }
      }

      startup_probe {
        http_get {
          path = "/healthz"
        }
        failure_threshold = 10
        period_seconds    = 3
      }

      liveness_probe {
        http_get {
          path = "/healthz"
        }
        period_seconds = 30
      }
    }
  }

  depends_on = [
    google_secret_manager_secret_iam_member.server_webhook,
    google_pubsub_topic_iam_member.server_publish,
  ]
}

# GitHub delivers webhooks unauthenticated, so the endpoint is public. What
# protects it is the signature check, which runs before the payload is parsed.
resource "google_cloud_run_v2_service_iam_member" "server_public" {
  name     = google_cloud_run_v2_service.server.name
  location = google_cloud_run_v2_service.server.location
  role     = "roles/run.invoker"
  member   = "allUsers"
}

# The worker pulls from the subscription, so it has no inbound traffic to scale
# on: it needs an instance that is always running with the CPU always granted.
resource "google_cloud_run_v2_service" "worker" {
  name     = "${var.name_prefix}-worker"
  location = var.region
  labels   = var.labels

  ingress             = "INGRESS_TRAFFIC_INTERNAL_ONLY"
  deletion_protection = false

  template {
    service_account = google_service_account.worker.email

    # A review runs for minutes after the last request, so the instance must
    # not be frozen between requests.
    max_instance_request_concurrency = 1

    scaling {
      min_instance_count = 1
      max_instance_count = var.worker_max_instances
    }

    containers {
      image = var.worker_image

      ports {
        container_port = 8081
      }

      resources {
        limits = {
          cpu    = "2"
          memory = "4Gi"
        }
        # Without this the CPU is throttled between requests and the pull
        # subscriber stops making progress.
        cpu_idle = false
      }

      env {
        name  = "KIBITZ_QUEUE_BACKEND"
        value = "pubsub"
      }
      env {
        name  = "KIBITZ_PUBSUB_PROJECT_ID"
        value = var.project_id
      }
      env {
        name  = "KIBITZ_PUBSUB_TOPIC"
        value = google_pubsub_topic.events.name
      }
      env {
        name  = "KIBITZ_PUBSUB_SUBSCRIPTION"
        value = google_pubsub_subscription.worker.name
      }
      env {
        name  = "KIBITZ_STATE_BACKEND"
        value = "firestore"
      }
      env {
        name  = "KIBITZ_FIRESTORE_PROJECT_ID"
        value = var.project_id
      }
      env {
        name  = "KIBITZ_WORKER_HEALTH_ADDR"
        value = ":8081"
      }
      env {
        name  = "KIBITZ_LOG_LEVEL"
        value = var.log_level
      }
      env {
        name  = "KIBITZ_CONCURRENCY"
        value = tostring(var.worker_concurrency)
      }
      env {
        name  = "KIBITZ_JOB_TIMEOUT"
        value = var.job_timeout
      }
      env {
        name  = "KIBITZ_MAX_DELIVERIES"
        value = tostring(var.max_delivery_attempts - 1)
      }
      env {
        name  = "KIBITZ_BOT_LOGINS"
        value = join(",", var.bot_logins)
      }
      env {
        name  = "KIBITZ_MODEL"
        value = var.model
      }
      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }
      env {
        name  = "VERTEX_LOCATION"
        value = var.vertex_location
      }
      env {
        name  = "KIBITZ_GITHUB_APP_ID"
        value = var.github_app_id
      }
      env {
        name  = "KIBITZ_GITHUB_INSTALLATION_ID"
        value = var.github_installation_id
      }
      env {
        name  = "KIBITZ_WORKSPACE_DIR"
        value = "/var/tmp/kibitz"
      }
      env {
        name = "KIBITZ_GITHUB_PRIVATE_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.github_private_key.secret_id
            version = "latest"
          }
        }
      }

      # Clones and agent scratch space go to memory-backed storage; the
      # container filesystem is small and the workspace is disposable anyway.
      volume_mounts {
        name       = "workspace"
        mount_path = "/var/tmp/kibitz"
      }

      startup_probe {
        http_get {
          path = "/healthz"
          port = 8081
        }
        failure_threshold = 10
        period_seconds    = 3
      }
    }

    volumes {
      name = "workspace"
      empty_dir {
        medium     = "MEMORY"
        size_limit = "2Gi"
      }
    }
  }

  depends_on = [
    google_secret_manager_secret_iam_member.worker_private_key,
    google_pubsub_subscription_iam_member.worker_subscribe,
    google_project_iam_member.worker_firestore,
    google_project_iam_member.worker_vertex,
  ]
}
