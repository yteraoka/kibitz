resource "google_pubsub_topic" "events" {
  name   = local.topic_name
  labels = var.labels

  # Long enough to replay a bad afternoon, short enough not to keep webhook
  # payloads around.
  message_retention_duration = "86400s"

  depends_on = [google_project_service.required]
}

# What reaches this topic is a message the worker could not decode, or one it
# never acknowledged. A job that fails repeatedly does not land here: kibitz
# gives up one attempt earlier and says so on the pull request instead
# (see docs/queue.md).
resource "google_pubsub_topic" "dead_letter" {
  name   = local.dead_letter_name
  labels = var.labels

  message_retention_duration = "604800s"

  depends_on = [google_project_service.required]
}

resource "google_pubsub_subscription" "worker" {
  name   = local.subscription_name
  topic  = google_pubsub_topic.events.id
  labels = var.labels

  # Events for one pull request are delivered in order, so a review of an old
  # commit cannot overtake a newer push.
  enable_message_ordering = true

  # The client extends this while a job runs; it only has to be long enough to
  # start one.
  ack_deadline_seconds = 60

  # A subscription that expires would silently stop delivering.
  expiration_policy {
    ttl = ""
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "600s"
  }

  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.dead_letter.id
    max_delivery_attempts = var.max_delivery_attempts
  }
}

# Nothing consumes the dead letter topic; this subscription exists so the
# messages are kept and can be inspected.
resource "google_pubsub_subscription" "dead_letter" {
  name   = "${local.dead_letter_name}-hold"
  topic  = google_pubsub_topic.dead_letter.id
  labels = var.labels

  message_retention_duration = "604800s"

  expiration_policy {
    ttl = ""
  }
}
