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

output "model_api_key_secret_name" {
  description = "Secret to add the model provider's API key to. Empty when the provider is Vertex AI, which needs no key."
  value       = var.model_api_key_env_name != "" ? google_secret_manager_secret.model_api_key[0].secret_id : ""
}

output "service_accounts" {
  description = "Identities the two components run as."
  value = {
    server = google_service_account.server.email
    worker = google_service_account.worker.email
    scaler = google_service_account.scaler.email
  }
}

output "worker_scaling" {
  description = "How the worker's instance count is decided. Terraform sets the starting point; kibitz-server and kibitz-scaler own it from then on."
  value = {
    service       = google_cloud_run_v2_service.worker.name
    scaler_job    = google_cloud_run_v2_job.scaler.name
    schedule      = google_cloud_scheduler_job.scaler.schedule
    min_instances = var.worker_min_instances
    max_instances = var.worker_max_instances
    idle_after    = var.worker_idle_after
  }
}

output "github_actions" {
  description = "What the release workflow needs as repository variables. None of it is secret."
  value = {
    GCP_PROJECT_ID                 = var.project_id
    GCP_REGION                     = var.region
    GCP_WORKLOAD_IDENTITY_PROVIDER = google_iam_workload_identity_pool_provider.github.name
    GCP_DEPLOY_SERVICE_ACCOUNT     = google_service_account.deployer.email
    GCP_IMAGE_REPOSITORY           = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
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
