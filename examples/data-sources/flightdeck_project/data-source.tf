# Look up a project by its identifier ...
data "flightdeck_project" "app" {
  identifier = "APP"
}

# ... or by numeric id.
data "flightdeck_project" "by_id" {
  id = 42
}

output "app_project_id" {
  value = data.flightdeck_project.app.id
}
