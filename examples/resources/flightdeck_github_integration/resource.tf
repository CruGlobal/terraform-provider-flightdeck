resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# Flightdeck-managed: the secret is generated and the repository webhook is
# registered through Flightdeck's GitHub App, which must be installed on the
# repository.
#
# `ci_failure_action` is `off` on a new integration, so failed workflow runs are
# accepted and dropped. Set it explicitly — the API keeps whatever is already
# there when the key is absent, so an unset attribute lets a choice made in the
# Flightdeck settings page stand with no diff to show for it.
resource "flightdeck_github_integration" "app" {
  project_id        = flightdeck_project.app.id
  repo_full_name    = "example-org/mobile-app"
  ci_failure_action = "file_intake"
}

# Caller-managed: you hold the secret and declare the repository webhook
# yourself, so the two sides share it.
resource "random_password" "flightdeck_webhook" {
  length  = 48
  special = false
}

resource "flightdeck_github_integration" "api" {
  project_id        = flightdeck_project.app.id
  repo_full_name    = "example-org/api"
  secret            = random_password.flightdeck_webhook.result
  ci_failure_action = "off"
}

resource "github_repository_webhook" "flightdeck" {
  repository = "api"
  events     = ["push", "pull_request"]

  configuration {
    url          = "https://flightdeck.example.com/integrations/github/webhooks"
    content_type = "json"
    secret       = random_password.flightdeck_webhook.result
  }
}
