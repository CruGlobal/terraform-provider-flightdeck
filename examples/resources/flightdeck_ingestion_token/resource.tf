resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  features = {
    errors = true
  }
}

resource "flightdeck_ingestion_token" "api_production" {
  project_id  = flightdeck_project.app.id
  name        = "api"
  environment = "production"
  scope       = "post_server_item"
}

# The token value is only returned on create; hand it to the application
# through a secret store rather than an output.
# output "api_production_token" {
#   value     = flightdeck_ingestion_token.api_production.token
#   sensitive = true
# }
