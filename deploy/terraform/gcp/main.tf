# Services kibitz needs. They are left enabled on destroy: disabling an API
# affects everything else in the project, not just kibitz.
resource "google_project_service" "required" {
  for_each = toset([
    "run.googleapis.com",
    "pubsub.googleapis.com",
    "firestore.googleapis.com",
    "secretmanager.googleapis.com",
    "aiplatform.googleapis.com",
    "artifactregistry.googleapis.com",
    "monitoring.googleapis.com",
    "cloudscheduler.googleapis.com",
  ])

  service            = each.value
  disable_on_destroy = false
}

data "google_project" "this" {
  depends_on = [google_project_service.required]
}

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = var.name_prefix
  format        = "DOCKER"
  description   = "kibitz container images"
  labels        = var.labels

  depends_on = [google_project_service.required]
}

locals {
  topic_name        = "${var.name_prefix}-events"
  dead_letter_name  = "${var.name_prefix}-events-dead"
  subscription_name = "${var.name_prefix}-worker"

  # The Pub/Sub service agent is what moves messages to the dead letter topic;
  # without these grants dead lettering silently does nothing.
  pubsub_agent = "serviceAccount:service-${data.google_project.this.number}@gcp-sa-pubsub.iam.gserviceaccount.com"
}
