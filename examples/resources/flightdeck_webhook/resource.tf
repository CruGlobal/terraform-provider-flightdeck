resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# Workspace-wide: every project's work item lifecycle.
resource "flightdeck_webhook" "ci" {
  url = "https://ci.example.com/hooks/flightdeck"
  events = [
    "work_item.created",
    "work_item.state_changed",
    "work_item.deleted",
  ]
}

# Scoped to one project. Flightdeck generates the HMAC signing secret and
# returns it once; hand it to the receiver from state (for example through a
# secret manager) rather than an output.
resource "flightdeck_webhook" "app_intake" {
  project_id = flightdeck_project.app.id
  url        = "https://intake.example.com/flightdeck"
  events     = ["intake.created", "intake.accepted", "intake.declined"]
}

# output "app_intake_signing_secret" {
#   value     = flightdeck_webhook.app_intake.secret
#   sensitive = true
# }
