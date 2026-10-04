# By id, which can never become ambiguous.
data "flightdeck_teamspace" "platform" {
  id = 12
}

# By name. Names are not unique, so this fails unless exactly one teamspace
# has the name. Letter case is ignored.
data "flightdeck_teamspace" "mobile" {
  name = "Mobile"
}

data "flightdeck_project" "app" {
  identifier = "APP"
}

# Say that a team managed elsewhere owns this project.
resource "flightdeck_teamspace_project" "app" {
  teamspace_id = data.flightdeck_teamspace.mobile.id
  project_id   = data.flightdeck_project.app.id
}
