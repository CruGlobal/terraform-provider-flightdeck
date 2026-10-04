resource "flightdeck_teamspace" "platform" {
  name = "Platform"
}

data "flightdeck_workspace_member" "alex" {
  email = "alex@example.com"
}

resource "flightdeck_teamspace_member" "alex" {
  teamspace_id = flightdeck_teamspace.platform.id
  user_id      = data.flightdeck_workspace_member.alex.id
}
