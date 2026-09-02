data "flightdeck_workspace_member" "deploy_bot" {
  email = "deploy-bot@example.com"
}

output "deploy_bot_user_id" {
  value = data.flightdeck_workspace_member.deploy_bot.id
}
