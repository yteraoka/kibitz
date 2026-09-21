resource "google_firestore_database" "state" {
  name        = "(default)"
  location_id = var.firestore_location
  type        = "FIRESTORE_NATIVE"

  # Deleting the database would lose the record of which deliveries have been
  # handled, so kibitz would review everything again. Destroying it has to be
  # a deliberate act.
  deletion_policy = "ABANDON"

  depends_on = [google_project_service.required]
}

# Expired documents are removed by Firestore itself. kibitz also checks the
# field on read, so a missing policy costs storage rather than correctness --
# but without it the collection grows forever.
resource "google_firestore_field" "ttl" {
  database   = google_firestore_database.state.name
  collection = var.name_prefix
  field      = "expiresAt"

  ttl_config {}

  # The single-field index on a TTL field is not useful and costs writes.
  index_config {}
}
