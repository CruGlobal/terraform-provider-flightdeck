resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# A login for the application, not a person. Write the address in lower
# case: Flightdeck stores it that way.
resource "flightdeck_service_account" "deploy" {
  email_address = "deploy-bot@machineusers.example.com"
  name          = "Deploy Bot"
}

# Give it only the project it works in. user_id takes the service account's id.
resource "flightdeck_project_member" "deploy" {
  project_id = flightdeck_project.app.id
  user_id    = flightdeck_service_account.deploy.id
  role       = "member"
}

# The token the application calls Flightdeck's API with.
resource "flightdeck_service_account_api_token" "deploy" {
  service_account_id = flightdeck_service_account.deploy.id
  name               = "Deploy production"
  expires_at         = "2027-06-01T00:00:00Z"
}

# The token value is only returned on create; hand it to the application
# through a secret store rather than an output.
# output "deploy_token" {
#   value     = flightdeck_service_account_api_token.deploy.token
#   sensitive = true
# }
