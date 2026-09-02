resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

resource "flightdeck_label" "security" {
  project_id = flightdeck_project.app.id
  name       = "Security"
  color      = "#dc2626"
}
