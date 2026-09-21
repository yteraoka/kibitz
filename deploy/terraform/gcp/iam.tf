# One identity per component: the server may publish and read its webhook
# secrets, the worker may consume, write state, call Vertex AI and read the
# GitHub App key. Neither can do the other's job.

resource "google_service_account" "server" {
  account_id   = "${var.name_prefix}-server"
  display_name = "kibitz webhook server"
}

resource "google_service_account" "worker" {
  account_id   = "${var.name_prefix}-worker"
  display_name = "kibitz review worker"
}

resource "google_pubsub_topic_iam_member" "server_publish" {
  topic  = google_pubsub_topic.events.name
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${google_service_account.server.email}"
}

resource "google_pubsub_subscription_iam_member" "worker_subscribe" {
  subscription = google_pubsub_subscription.worker.name
  role         = "roles/pubsub.subscriber"
  member       = "serviceAccount:${google_service_account.worker.email}"
}

# The worker keeps its state in Firestore. datastore.user is the role that
# grants document reads and writes; there is no narrower one.
resource "google_project_iam_member" "worker_firestore" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.worker.email}"
}

# Vertex AI, which is why the worker holds no model API key at all.
resource "google_project_iam_member" "worker_vertex" {
  project = var.project_id
  role    = "roles/aiplatform.user"
  member  = "serviceAccount:${google_service_account.worker.email}"
}

# Traces and metrics.
resource "google_project_iam_member" "telemetry" {
  for_each = {
    server_metrics = { role = "roles/monitoring.metricWriter", member = google_service_account.server.email }
    worker_metrics = { role = "roles/monitoring.metricWriter", member = google_service_account.worker.email }
    server_traces  = { role = "roles/cloudtrace.agent", member = google_service_account.server.email }
    worker_traces  = { role = "roles/cloudtrace.agent", member = google_service_account.worker.email }
  }

  project = var.project_id
  role    = each.value.role
  member  = "serviceAccount:${each.value.member}"
}

# Dead lettering is done by the Pub/Sub service agent on kibitz's behalf.
resource "google_pubsub_topic_iam_member" "dead_letter_publish" {
  topic  = google_pubsub_topic.dead_letter.name
  role   = "roles/pubsub.publisher"
  member = local.pubsub_agent
}

resource "google_pubsub_subscription_iam_member" "dead_letter_subscribe" {
  subscription = google_pubsub_subscription.worker.name
  role         = "roles/pubsub.subscriber"
  member       = local.pubsub_agent
}
