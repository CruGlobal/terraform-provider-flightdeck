resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  features = {
    incidents = true
  }
}

# One key per monitor, so a compromised or retired monitor can be revoked on
# its own. The key opens incidents in this project; it pages nobody unless an
# escalation policy is attached to it from the Flightdeck console.
resource "flightdeck_routing_key" "uptime" {
  project_id = flightdeck_project.app.id
  name       = "Uptime monitor"
}

resource "flightdeck_routing_key" "synthetics" {
  project_id = flightdeck_project.app.id
  name       = "Synthetic checks"
}

# A key whose incidents can also trip the project's self-healing loop, beside
# the error rate. It counts only with the environment "production", and only
# while the project's self_healing.monitor_rollback isn't "off" (see
# flightdeck_project). Put only server-side monitors on it, never browser RUM.
resource "flightdeck_routing_key" "checkout_latency" {
  project_id           = flightdeck_project.app.id
  name                 = "Checkout latency"
  environment          = "production"
  self_healing_trigger = true
}

# Sending a Datadog monitor's alerts with that key, using the Datadog provider.
# The key goes in a hidden custom variable, and one webhook posts every alert
# transition to Flightdeck's Events API, so a recovery resolves the incident.
# Mention @webhook-flightdeck-checkout in the monitor's message, outside any
# {{#is_alert}} block, so every transition reaches it.
resource "datadog_webhook_custom_variable" "checkout_latency_key" {
  name      = "FLIGHTDECK_CHECKOUT_ROUTING_KEY"
  value     = flightdeck_routing_key.checkout_latency.routing_key
  is_secret = true
}

resource "datadog_webhook" "flightdeck_checkout" {
  name      = "flightdeck-checkout"
  url       = "https://flightdeck.example.com/api/1/events"
  encode_as = "json"
  payload = jsonencode({
    routing_key  = "$FLIGHTDECK_CHECKOUT_ROUTING_KEY"
    event_action = "$ALERT_TRANSITION"
    # A trip key's alerts use the alert cycle as the dedup key, so each cycle
    # from Triggered to Recovered opens its own incident.
    dedup_key = "$ALERT_CYCLE_KEY"
    summary   = "$EVENT_TITLE"
    source    = "$LINK"
    severity  = "$ALERT_TYPE"
    monitor   = "$ALERT_ID"
  })

  depends_on = [datadog_webhook_custom_variable.checkout_latency_key]
}

# The value exists only in state, because the API returns it once. Hand it to
# whatever needs it rather than printing it.
output "uptime_routing_key" {
  value     = flightdeck_routing_key.uptime.routing_key
  sensitive = true
}
