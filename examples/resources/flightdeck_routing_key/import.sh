# Import with "<project_id>/<key_id>". An imported key has no `routing_key`
# value: the API returns it only when the key is created.
terraform import flightdeck_routing_key.uptime 42/7
