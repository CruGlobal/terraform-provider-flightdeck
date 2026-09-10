# Resolve people by email, so the configuration says who has access rather
# than which user id. The match is exact: case and surrounding whitespace are
# ignored, but a partial address resolves to nobody and fails the plan.
data "flightdeck_workspace_member" "lead" {
  email = "ada@example.com"
}

# Service accounts resolve only for a workspace admin, and `kind` is what
# tells one apart from a person with a similar-looking address.
data "flightdeck_workspace_member" "deploy_bot" {
  email = "deploy-bot@example.com"
}

resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  lead_id    = data.flightdeck_workspace_member.lead.id
}

resource "flightdeck_project_member" "deploy_bot" {
  project_id = flightdeck_project.app.id
  user_id    = data.flightdeck_workspace_member.deploy_bot.id
  role       = "member"

  lifecycle {
    precondition {
      condition     = data.flightdeck_workspace_member.deploy_bot.kind == "service"
      error_message = "Expected a service account, not a person."
    }
  }
}
