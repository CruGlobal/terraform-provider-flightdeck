# Import with "<project_id>/<membership_id>" ...
terraform import flightdeck_project_member.deploy_bot 42/118

# ... or look the membership up by user: "<project_id>/user:<user_id>".
terraform import flightdeck_project_member.deploy_bot 42/user:7
