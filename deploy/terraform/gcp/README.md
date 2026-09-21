# kibitz on Google Cloud

Terraform for the GCP deployment: Cloud Run for both binaries, Pub/Sub between
them, Firestore for state, Secret Manager for credentials, and alerts for the
ways kibitz fails quietly.

The step-by-step procedure, including creating the GitHub App and the first
smoke test, is in [docs/deployment.md](../../../docs/deployment.md). This file
covers what the configuration itself does.

## What is created

| Resource | Why |
| --- | --- |
| `google_cloud_run_v2_service.server` | Public: GitHub has to reach it. Scales to zero; the signature check is what protects it |
| `google_cloud_run_v2_service.worker` | Internal, always one instance with the CPU always allocated, because a pull subscriber has no requests to scale on |
| `google_pubsub_subscription.worker` | Ordered per pull request, with a dead letter policy |
| `google_firestore_database.state` | Idempotency records and locks. `deletion_policy = ABANDON`: losing it means reviewing everything again |
| `google_secret_manager_secret.*` | The webhook secret and the App key. Values are added with gcloud, so they never enter the Terraform state |
| `google_monitoring_alert_policy.*` | Dead letters, a backlog that will not drain, and a server refusing deliveries |

Each component runs as its own service account: the server may publish and read
the webhook secret, the worker may consume, write state, call Vertex AI and
read the App key. Neither can do the other's job.

## Applying it

The first apply is staged, because images have to be pushed to a registry this
configuration creates, and Cloud Run will not start without a secret version to
mount. See [docs/deployment.md](../../../docs/deployment.md) §3–5.

## Notes

- **Firestore's location cannot be changed** after the database is created.
- `max_delivery_attempts` is Pub/Sub's threshold. The worker gives up one
  attempt earlier and reports the failure on the pull request, so a message
  that keeps failing is visible to the author rather than only in a dead letter
  queue. See [docs/queue.md](../../../docs/queue.md).
- Without `alert_notification_channels`, the alert policies exist but notify
  nobody.
