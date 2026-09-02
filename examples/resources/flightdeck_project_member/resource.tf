resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# Say who, not which id.
data "flightdeck_workspace_member" "deploy_bot" {
  email = "deploy-bot@example.com"
}

resource "flightdeck_project_member" "deploy_bot" {
  project_id = flightdeck_project.app.id
  user_id    = data.flightdeck_workspace_member.deploy_bot.id
  role       = "member"
}
