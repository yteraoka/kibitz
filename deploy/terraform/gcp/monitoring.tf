# Alerts are about the two ways kibitz fails quietly: it stops accepting work,
# or it accepts work and drops it.

# A message in the dead letter topic is either undecodable or one the worker
# never acknowledged. Either way a review was lost.
resource "google_monitoring_alert_policy" "dead_letter" {
  display_name = "${var.name_prefix}: messages in the dead letter topic"
  combiner     = "OR"

  conditions {
    display_name = "dead letter topic received a message"

    condition_threshold {
      filter = join(" AND ", [
        "resource.type = \"pubsub_topic\"",
        "resource.labels.topic_id = \"${google_pubsub_topic.dead_letter.name}\"",
        "metric.type = \"pubsub.googleapis.com/topic/send_message_operation_count\"",
      ])
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "0s"

      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  notification_channels = var.alert_notification_channels

  documentation {
    content = join("\n", [
      "A message reached kibitz's dead letter topic, so a review was lost.",
      "",
      "Inspect it with:",
      "  gcloud pubsub subscriptions pull ${google_pubsub_subscription.dead_letter.name} --limit=10 --format=json",
      "",
      "The usual causes are a message written by a newer build than the worker",
      "is running, and a worker that stopped acknowledging.",
    ])
  }
}

# A backlog that keeps growing means the workers are not keeping up, or are
# not running at all.
resource "google_monitoring_alert_policy" "backlog" {
  display_name = "${var.name_prefix}: review backlog is not draining"
  combiner     = "OR"

  conditions {
    display_name = "oldest unacknowledged message is over 30 minutes old"

    condition_threshold {
      filter = join(" AND ", [
        "resource.type = \"pubsub_subscription\"",
        "resource.labels.subscription_id = \"${google_pubsub_subscription.worker.name}\"",
        "metric.type = \"pubsub.googleapis.com/subscription/oldest_unacked_message_age\"",
      ])
      comparison      = "COMPARISON_GT"
      threshold_value = 1800
      duration        = "300s"

      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_MAX"
      }
    }
  }

  notification_channels = var.alert_notification_channels

  documentation {
    content = join("\n", [
      "Events are arriving faster than they are being reviewed, or the worker",
      "is down. Check the worker's logs and whether it is holding a lock it",
      "cannot release:",
      "  gcloud run services logs read ${var.name_prefix}-worker --region ${var.region}",
    ])
  }
}

# The server answering 5xx means deliveries are being refused, and GitHub does
# not retry them on its own.
resource "google_monitoring_alert_policy" "server_errors" {
  display_name = "${var.name_prefix}: the webhook server is refusing deliveries"
  combiner     = "OR"

  conditions {
    display_name = "5xx responses from the server"

    condition_threshold {
      filter = join(" AND ", [
        "resource.type = \"cloud_run_revision\"",
        "resource.labels.service_name = \"${google_cloud_run_v2_service.server.name}\"",
        "metric.type = \"run.googleapis.com/request_count\"",
        "metric.labels.response_code_class = \"5xx\"",
      ])
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "300s"

      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  notification_channels = var.alert_notification_channels

  documentation {
    content = join("\n", [
      "The server could not publish, so webhooks were refused. GitHub does not",
      "redeliver on its own: the failed deliveries have to be replayed from",
      "the repository's webhook settings once the cause is fixed.",
    ])
  }
}
