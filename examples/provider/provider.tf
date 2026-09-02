terraform {
  required_providers {
    flightdeck = {
      source  = "CruGlobal/flightdeck"
      version = "~> 0.1"
    }
  }
}

# Both attributes fall back to FLIGHTDECK_ENDPOINT / FLIGHTDECK_TOKEN, so the
# token never has to appear in configuration.
provider "flightdeck" {
  endpoint = "https://flightdeck.example.com"
}

# Or set them explicitly (the token should come from a variable or secret store).
# provider "flightdeck" {
#   endpoint = "https://flightdeck.example.com"
#   token    = var.flightdeck_token
# }
