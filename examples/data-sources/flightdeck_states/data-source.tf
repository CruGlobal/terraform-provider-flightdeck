data "flightdeck_project" "app" {
  identifier = "APP"
}

data "flightdeck_states" "app" {
  project_id = data.flightdeck_project.app.id
}

# Map state names to ids, e.g. to look up the "Done" state.
output "done_state_id" {
  value = one([for s in data.flightdeck_states.app.states : s.id if s.name == "Done"])
}
