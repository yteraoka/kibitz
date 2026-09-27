terraform {
  required_version = ">= 1.6"

  required_providers {
    google = {
      source = "hashicorp/google"
      # 7.x is what can say the server's instance limit where Cloud Run and
      # gcloud keep it: 6.x has no service-level max_instance_count (see the
      # server in cloudrun.tf).
      version = "~> 7.0"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
