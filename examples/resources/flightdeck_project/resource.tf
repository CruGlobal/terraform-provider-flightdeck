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

# Self-healing thresholds (workspace admins only). `armed` is read-only:
# arming a project stays a console operation.
resource "flightdeck_project" "payments" {
  name       = "Payments"
  identifier = "PAY"

  self_healing = {
    bake_minutes           = 30
    burn_rate              = 10.0
    max_rollbacks_per_hour = 2
  }
}
