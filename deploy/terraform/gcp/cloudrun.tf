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
      max_instance_count = var.server_max_instances
    }

    containers {
      image = var.server_image

      ports {
        container_port = 8080
      }

      # Sized for what the server actually does: verify a signature, publish
      # one message, return. The boost is what covers the cold start, which
      # is the only moment the CPU limit is felt -- a webhook that waits for
      # one risks the forge's timeout.
      #
      # A whole CPU is the floor here, not a measurement: Cloud Run only
      # allows a fractional CPU when an instance takes one request at a time,
      # and this service keeps the default concurrency so one warm instance
      # absorbs a burst of deliveries. Asking for less is rejected at deploy
      # time with "Total cpu < 1 is not supported with concurrency > 1".
      #
      # 512Mi is likewise the floor: the second generation execution
      # environment refuses to start below it.
      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
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
      # Not a secret: it is the number in the app's settings URL. It is how
      # the server recognizes comments kibitz itself wrote, which GitHub
      # attributes to the app that made them. Matching on that rather than on
      # an account name means nothing to keep in step and nothing that breaks
      # when the app is renamed.
      env {
        name  = "KIBITZ_GITHUB_APP_ID"
        value = var.github_app_id
      }
      env {
        name  = "KIBITZ_MENTION"
        value = var.mention
      }
      env {
        name  = "KIBITZ_TRIGGER_KEYWORDS"
        value = join(",", var.trigger_keywords)
      }
      # The worker is allowed to sit at zero instances, so publishing is not
      # enough on its own: the server starts it as soon as it queues
      # something, rather than leaving the review to wait for the next
      # backlog sample. The scaler handles the way back down.
      env {
        name  = "KIBITZ_SCALE_BACKEND"
        value = "cloudrun"
      }
      env {
        name  = "KIBITZ_SCALE_REGION"
        value = var.region
      }
      env {
        name  = "KIBITZ_SCALE_WORKER_SERVICE"
        value = "${var.name_prefix}-worker"
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
    google_cloud_run_v2_service_iam_member.worker_scaling,
    google_service_account_iam_member.worker_act_as,
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

# The worker pulls from the subscription, so it has no inbound traffic for
# Cloud Run to scale on. kibitz supplies the missing signal itself: the server
# starts it when it publishes, and kibitz-scaler sizes it from the backlog and
# returns it to zero once the queue has been empty for a while. See
# autoscale.tf and docs/deployment.md.
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

    # The revision template's floor stays at zero. The count that actually
    # applies is the service-level one below, which is the only one that can
    # be changed without rolling a new revision (and interrupting a review).
    scaling {
      min_instance_count = 0
      max_instance_count = var.worker_max_instances
    }

    containers {
      image = var.worker_image

      ports {
        container_port = 8081
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "2Gi"
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
      dynamic "env" {
        for_each = var.vertex_maas_base_url != "" ? [var.vertex_maas_base_url] : []
        content {
          name  = "KIBITZ_VERTEX_MAAS_BASE_URL"
          value = env.value
        }
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

      # A provider that authenticates with a key gets it under the name that
      # provider looks for, and kibitz is told to forward that name to the
      # agent. The value never appears in kibitz's own configuration.
      dynamic "env" {
        for_each = var.model_api_key_env_name != "" ? [var.model_api_key_env_name] : []
        content {
          name = env.value
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.model_api_key[0].secret_id
              version = "latest"
            }
          }
        }
      }

      dynamic "env" {
        for_each = var.model_api_key_env_name != "" ? [var.model_api_key_env_name] : []
        content {
          name  = "KIBITZ_PROVIDER_ENV"
          value = env.value
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

  # Service-level scaling is what the server and the scaler write at runtime.
  # Terraform sets the starting point and then leaves it alone: the two would
  # otherwise undo each other on every apply.
  scaling {
    min_instance_count = var.worker_min_instances
  }

  lifecycle {
    ignore_changes = [scaling]
  }

  depends_on = [
    google_secret_manager_secret_iam_member.worker_private_key,
    google_secret_manager_secret_iam_member.worker_model_api_key,
    google_pubsub_subscription_iam_member.worker_subscribe,
    google_project_iam_member.worker_firestore,
    google_project_iam_member.worker_vertex,
  ]
}
