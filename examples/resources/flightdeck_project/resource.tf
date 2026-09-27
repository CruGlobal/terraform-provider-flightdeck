resource "flightdeck_project" "app" {
  name        = "Mobile App"
  identifier  = "APP"
  description = "The customer-facing mobile application"
  emoji       = "📱"
  network     = "private_project" # explicit members only; new projects are public

  # Only the feature keys listed here are managed; others keep their value.
  features = {
    intake = true
    errors = true
  }
}

# Self-healing (workspace admins only). `rollback` is the loop's mode:
# "report" notes what it would do and never touches production, and "auto"
# lets Flightdeck roll production back by itself when every check passes.
resource "flightdeck_project" "payments" {
  name       = "Payments"
  identifier = "PAY"

  self_healing = {
    feature_enabled        = true
    rollback               = "report"
    bake_minutes           = 30
    burn_rate              = 10.0
    max_rollbacks_per_hour = 2
  }
}

# A project's Slack channel (project admins). Provisioning is asynchronous:
# enabling the channel enqueues the job, and `channel_id` and
# `provision_status` are filled in once it has run.
resource "flightdeck_project" "support" {
  name       = "Support"
  identifier = "SUP"

  slack_channel = {
    enabled = true
    name    = "team-support" # omit to use the project-name default

    # Only the categories listed here are managed; the API merges them, so
    # removing one leaves it as it is rather than restoring its default.
    event_filter = {
      logged        = true
      field_changed = false
    }
  }
}
