resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  features = {
    incidents = true
  }
}

# One key per monitor, so a compromised or retired monitor can be revoked on
# its own. The key opens incidents in this project; it pages nobody unless an
# escalation policy is attached to it from the Flightdeck console.
resource "flightdeck_routing_key" "uptime" {
  project_id = flightdeck_project.app.id
  name       = "Uptime monitor"
}

resource "flightdeck_routing_key" "synthetics" {
  project_id = flightdeck_project.app.id
  name       = "Synthetic checks"
}

# The value exists only in state, because the API returns it once. Hand it to
# whatever needs it rather than printing it.
output "uptime_routing_key" {
  value     = flightdeck_routing_key.uptime.routing_key
  sensitive = true
}
