resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
}

# user_id is the workspace member's numeric id (the API has no route to look a
# user up by email). Find it in the workspace's member list.
resource "flightdeck_project_member" "deploy_bot" {
  project_id = flightdeck_project.app.id
  user_id    = 7
  role       = "member"
}
