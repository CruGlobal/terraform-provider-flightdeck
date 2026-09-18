resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  features = {
    incidents = true
  }
}

# `routing_key` is write-only: Terraform sends it on apply and never writes it
# to the plan or the state file. Feed it from something Terraform does not
# persist either — an ephemeral variable, or an ephemeral resource reading your
# secret manager.
variable "pagerduty_routing_key" {
  type      = string
  ephemeral = true
  sensitive = true
}

resource "flightdeck_pagerduty_integration" "app" {
  project_id = flightdeck_project.app.id

  routing_key = var.pagerduty_routing_key

  # Bump this to force the key to be re-sent when a rotation is invisible to
  # the last-four comparison — a new key ending in the same four characters as
  # the old one.
  routing_key_version = "2026-09-17"

  # Forward anything at "error" or above; warnings and info stay in Flightdeck.
  min_severity = "error"

  # Display-only, so the link in Flightdeck's UI goes somewhere useful.
  service_id  = "PABC123"
  service_url = "https://example.pagerduty.com/service-directory/PABC123"
}
