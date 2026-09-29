resource "flightdeck_project" "app" {
  name       = "Web App"
  identifier = "APP"
  features = {
    errors    = true
    incidents = true
  }
}

# Post every never-seen production error at level "error" or above to Slack.
resource "flightdeck_error_alert_rule" "new_errors" {
  project_id = flightdeck_project.app.id
  name       = "New production errors"
  trigger    = "new_group"

  condition = {
    min_level   = "error"
    environment = "production"
  }

  action = {
    notify_slack     = true
    create_work_item = true
  }
}

# Page an external system when an error recurs 50 times in 10 minutes.
resource "flightdeck_error_alert_rule" "error_storm" {
  project_id = flightdeck_project.app.id
  name       = "Error storm"
  trigger    = "occurrence_threshold"

  condition = {
    count          = 50
    window_minutes = 10
  }

  action = {
    notify_webhook = true
    webhook_url    = "https://alerts.example.com/hooks/flightdeck"
  }
}

# Open an incident for a new production error, leaving out errors sent with the
# browser token. That token ships inside every page, so anyone can read it and
# post a fake error; leaving those out means one can't page anybody. The
# trade-off: if a browser error creates the group first, this rule stays silent
# for that error (see count_browser_errors below).
resource "flightdeck_error_alert_rule" "page_on_call" {
  project_id = flightdeck_project.app.id
  name       = "Page on new server errors"
  trigger    = "new_group"

  condition = {
    min_level            = "error"
    environment          = "production"
    count_browser_errors = false
  }

  action = {
    open_incident = true
  }
}
