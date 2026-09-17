resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  features = {
    errors    = true
    incidents = true
  }
}

# Tell the team's Slack channel whenever an incident opens at "error" or above,
# and file the work item that tracks it.
resource "flightdeck_incident_alert_rule" "opened" {
  project_id = flightdeck_project.app.id
  name       = "Incident opened"
  trigger    = "incident_opened"

  condition = {
    min_severity = "error"
  }

  action = {
    notify_slack     = true
    create_work_item = true
  }
}

# A critical incident that keeps repeating is worth more than a Slack message:
# raise what it files to "urgent" and post it to an external system too.
resource "flightdeck_incident_alert_rule" "repeating" {
  project_id = flightdeck_project.app.id
  name       = "Incident will not settle"
  trigger    = "incident_repeated"

  condition = {
    min_severity   = "critical"
    count          = 5
    window_minutes = 30
  }

  action = {
    notify_webhook = true
    webhook_url    = "https://alerts.example.com/hooks/flightdeck"

    # Only the severities named here are managed; the rest keep their defaults.
    priority_map = {
      critical = "urgent"
      error    = "high"
    }
  }
}
