resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# Appended to the end of the "started" group.
resource "flightdeck_state" "in_review" {
  project_id = flightdeck_project.app.id
  name       = "In Review"
  group      = "started"
  color      = "#8b5cf6"
}

# Becomes the state new work items land in (the previous default is cleared).
resource "flightdeck_state" "triage" {
  project_id = flightdeck_project.app.id
  name       = "Triage"
  group      = "backlog"
  default    = true
}
