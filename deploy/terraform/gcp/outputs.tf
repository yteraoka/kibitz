output "webhook_url" {
  description = "The URL to configure in the GitHub App. Only the /webhook/github path accepts deliveries."
  value       = "${google_cloud_run_v2_service.server.uri}/webhook/github"
}

output "server_url" {
  description = "Base URL of the webhook server."
  value       = google_cloud_run_v2_service.server.uri
}

output "image_repository" {
  description = "Artifact Registry repository to push images to."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "webhook_secret_name" {
  description = "Secret to add the GitHub webhook secret to."
  value       = google_secret_manager_secret.github_webhook.secret_id
}

output "github_private_key_secret_name" {
  description = "Secret to add the GitHub App private key to."
  value       = google_secret_manager_secret.github_private_key.secret_id
}

output "service_accounts" {
  description = "Identities the two components run as."
  value = {
    server = google_service_account.server.email
    worker = google_service_account.worker.email
  }
}

output "pubsub" {
  description = "Queue resources."
  value = {
    topic        = google_pubsub_topic.events.name
    subscription = google_pubsub_subscription.worker.name
    dead_letter  = google_pubsub_topic.dead_letter.name
  }
}
