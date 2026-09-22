# kibitz on Google Cloud

Terraform for the GCP deployment: Cloud Run for both binaries -- a service for
the webhook, a worker pool for the reviews -- Pub/Sub between them, Firestore
for state, Secret Manager for credentials, and alerts for the ways kibitz
fails quietly.

The step-by-step procedure, including creating the GitHub App and the first
smoke test, is in [docs/deployment.md](../../../docs/deployment.md). This file
covers what the configuration itself does.

## What is created

| Resource | Why |
| --- | --- |
| `google_cloud_run_v2_service.server` | Public: GitHub has to reach it. Scales to zero; the signature check is what protects it |
| `google_cloud_run_v2_worker_pool.worker` | A pull subscriber receives no requests, which is what a worker pool is for: no ingress, no ports, no probes, and the CPU allocated for the life of the instance. Its instance count is written at runtime, not by Terraform |
| `google_cloud_run_v2_job.scaler` + `google_cloud_scheduler_job.scaler` | Sizes the worker from the Pub/Sub backlog once a minute, and returns it to `worker_min_instances` (zero by default) once the queue has been empty for `worker_idle_after` |
| `google_pubsub_subscription.worker` | Ordered per pull request, with a dead letter policy |
| `google_firestore_database.state` | Idempotency records and locks. `deletion_policy = ABANDON`: losing it means reviewing everything again |
| `google_secret_manager_secret.*` | The webhook secret and the App key. Values are added with gcloud, so they never enter the Terraform state |
| `google_monitoring_alert_policy.*` | Dead letters, a backlog that will not drain, a server refusing deliveries, and an autoscaler that keeps failing |

Each component runs as its own service account: the server may publish and read
the webhook secret, the worker may consume, write state, call Vertex AI and
read the App key, and the scaler may read Monitoring. None can do another's
job. The server and the scaler also hold `roles/run.developer` **on the worker
pool alone**, which is what lets them change its instance count and nothing
else.

## Who owns the images

Terraform sets each image once and then ignores the field, because the release
workflow deploys it on a tag push. Whoever applied last would
otherwise undo the other. `terraform output github_actions` prints what that
workflow needs; none of it is secret, so it goes in repository variables
rather than secrets.

The deploy service account's grants are per resource -- push to this Artifact
Registry repository, and `roles/run.developer` on the server, the worker pool
and the scaler job -- with one exception. Updating a worker pool returns a
long-running operation that gcloud polls, and that operation is not a child of
the pool, so reading it takes a project-level permission: a custom role holding
`run.operations.get` and nothing else.

The workflow authenticates with Workload Identity Federation, so no service
account key exists to leak. The pool is an existing one that Terraform only
reads (`workload_identity_pool_id`, `github-pool` by default), so it can be
shared with anything else federating into the project; the provider inside it
is kibitz's, and its condition is what narrows access. What may use it is this
repository, and only on a tag: a workflow on a branch cannot exchange a token
at all.

## Who owns the worker's instance count

A worker pool scales manually -- there is no traffic to scale it on -- so the
count is something kibitz writes. Terraform sets
`scaling.manual_instance_count` once and then ignores it
(`lifecycle { ignore_changes = [scaling] }`). From then on it belongs to
kibitz: the server sets it to one when it publishes, and the scaler moves it
up and back down from the backlog. Without the ignore rule the next
`terraform apply` would undo whatever the scaler had decided.

`worker_max_instances` is not part of the worker pool at all. Nothing but
kibitz moves this number, so the cap lives where the number is decided: in the
scaler.

The count is written on the pool rather than on its revision template, because
changing the template rolls a new revision, which would interrupt a review in
progress.

## Terraform version

The repository pins Terraform with [mise](https://mise.jdx.dev); `mise install`
at the repository root gives you the version this configuration is developed
against (`mise.toml`). `versions.tf` keeps a lower bound rather than a pin, so
the module still works for anyone not using mise.

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
