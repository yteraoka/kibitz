# The record of what the agent did for each job: its prompt and every event
# its engine reported. The worker writes one per job and names it in a
# trailer on the comment it posts; the server renders it at
# /transcripts/{id}. See docs/adr/0026-keep-what-the-agent-did-behind-the-comment.md.
#
# Off with transcripts_enabled = false, which leaves the bucket, the grants
# and the trailer out altogether.

locals {
  transcripts_enabled = var.transcripts_enabled

  # A password only means something in front of records that exist.
  transcript_password_enabled = local.transcripts_enabled && var.transcript_password_enabled

  # The server's address, written out rather than read from the service: the
  # server already depends on the worker pool, and the worker reading the
  # server's uri would make a cycle. Cloud Run's deterministic URL is stable
  # for as long as the service keeps its name.
  server_url = "https://${var.name_prefix}-server-${data.google_project.this.number}.${var.region}.run.app"
}

resource "google_storage_bucket" "transcripts" {
  count = local.transcripts_enabled ? 1 : 0

  name     = "${var.project_id}-${var.name_prefix}-transcripts"
  location = var.region
  labels   = var.labels

  # A transcript is mostly the repository's own code read back through a
  # tool. It is kept long enough to answer "why did it say that", and no
  # longer.
  lifecycle_rule {
    condition {
      age = var.transcript_retention_days
    }
    action {
      type = "Delete"
    }
  }

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  force_destroy = true

  depends_on = [google_project_service.required]
}

# The worker writes the records. objectUser rather than objectCreator: a job
# that posts twice (an issue comment and the pull request it opened) saves
# the same record again with more in it, which is an overwrite.
resource "google_storage_bucket_iam_member" "worker_transcripts" {
  count = local.transcripts_enabled ? 1 : 0

  bucket = google_storage_bucket.transcripts[0].name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${google_service_account.worker.email}"
}

# The server only reads them.
resource "google_storage_bucket_iam_member" "server_transcripts" {
  count = local.transcripts_enabled ? 1 : 0

  bucket = google_storage_bucket.transcripts[0].name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.server.email}"
}

# The passwords the server asks for before it shows a record, comma
# separated so that a rotation can hold the old and the new one at once. The
# value goes in with gcloud, never through Terraform, so it stays out of the
# state; the server reads it as KIBITZ_TRANSCRIPT_TOKENS.
resource "google_secret_manager_secret" "transcript_tokens" {
  count = local.transcript_password_enabled ? 1 : 0

  secret_id = "${var.name_prefix}-transcript-tokens"
  labels    = var.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

# Only the server checks the password. The worker writes records and links
# to them, and never needs to know it.
resource "google_secret_manager_secret_iam_member" "server_transcript_tokens" {
  count = local.transcript_password_enabled ? 1 : 0

  secret_id = google_secret_manager_secret.transcript_tokens[0].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.server.email}"
}
