# The record of what the agent did for each job: its prompt and every event
# its engine reported. The worker writes one per job and names it in a
# trailer on the comment it posts; the server renders it at
# /transcripts/{id}. See docs/adr/0026-keep-what-the-agent-did-behind-the-comment.md.
#
# Off with transcripts_enabled = false, which leaves the bucket, the grants
# and the trailer out altogether.

locals {
  transcripts_enabled = var.transcripts_enabled

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
