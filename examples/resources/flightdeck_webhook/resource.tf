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

# Scoped to one project, with a secret you manage yourself.
resource "flightdeck_webhook" "app_intake" {
  project_id = flightdeck_project.app.id
  url        = "https://intake.example.com/flightdeck"
  events     = ["intake.created", "intake.accepted", "intake.declined"]
  secret     = var.webhook_secret
}

variable "webhook_secret" {
  type      = string
  sensitive = true
}
