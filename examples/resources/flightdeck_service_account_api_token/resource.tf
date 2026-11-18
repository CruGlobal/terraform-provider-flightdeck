resource "flightdeck_service_account" "deploy" {
  email_address = "deploy-bot@machineusers.example.com"
  name          = "Deploy Bot"
}

resource "flightdeck_service_account_api_token" "deploy" {
  service_account_id = flightdeck_service_account.deploy.id
  name               = "Deploy production"
  # Leave expires_at out for a token that never expires. To rotate the
  # token, set a later time: that replaces it.
  expires_at = "2027-06-01T00:00:00Z"

  # Make the new token before deleting the old one, so the application is
  # not cut off while it waits for the new value.
  lifecycle {
    create_before_destroy = true
  }
}

# The token value is only returned on create; hand it to the application
# through a secret store rather than an output.
# output "deploy_token" {
#   value     = flightdeck_service_account_api_token.deploy.token
#   sensitive = true
# }
