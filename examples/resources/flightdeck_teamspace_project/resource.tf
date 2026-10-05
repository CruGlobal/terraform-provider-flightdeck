resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

resource "flightdeck_teamspace" "platform" {
  name = "Platform"
}

# The team owns the project. The token needs read and administer on the
# project to link it.
resource "flightdeck_teamspace_project" "app" {
  teamspace_id = flightdeck_teamspace.platform.id
  project_id   = flightdeck_project.app.id
}
