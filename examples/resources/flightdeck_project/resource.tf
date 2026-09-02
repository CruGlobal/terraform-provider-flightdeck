resource "flightdeck_project" "app" {
  name        = "Mobile App"
  identifier  = "APP"
  description = "The customer-facing mobile application"
  emoji       = "📱"

  # Only the feature keys listed here are managed; others keep their value.
  features = {
    intake = true
    errors = true
  }

  github_repo_full_name = "example-org/mobile-app"
}
