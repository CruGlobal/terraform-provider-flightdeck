# lead_id takes a user id. Resolve it from an email address.
data "flightdeck_workspace_member" "lead" {
  email = "lead@example.com"
}

resource "flightdeck_teamspace" "platform" {
  name        = "Platform"
  description = "Pipelines and shared infrastructure"
  lead_id     = data.flightdeck_workspace_member.lead.id
}
