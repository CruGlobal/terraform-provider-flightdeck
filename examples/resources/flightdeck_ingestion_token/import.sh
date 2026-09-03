# Import with "<project_id>/<token_id>" (or a bare token id, which searches
# every readable project). An imported token has no `token` value, and a
# revoked token cannot be imported.
terraform import flightdeck_ingestion_token.api_production 42/31
