resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# user_id is the workspace member's numeric id. Resolve it from an email
# address rather than hard-coding it.
data "flightdeck_workspace_member" "deploy_bot" {
  email = "deploy-bot@example.com"
}

resource "flightdeck_project_member" "deploy_bot" {
  project_id = flightdeck_project.app.id
  user_id    = data.flightdeck_workspace_member.deploy_bot.id
  role       = "member"
}
