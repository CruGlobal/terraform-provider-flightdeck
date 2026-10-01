resource "flightdeck_project" "app" {
  name        = "Mobile App"
  identifier  = "APP"
  description = "The customer-facing mobile application"
  emoji       = "📱"
  network     = "private_project" # explicit members only; new projects are public

  # The deployed app this project belongs to (needs a workspace owner or admin
  # token). Optional: the first release event that names an app binds it.
  app = "mobile-app"

  # Only the feature keys listed here are managed; others keep their value.
  features = {
    intake = true
    errors = true
  }
}

# Self-healing (workspace admins only). `rollback` is the loop's mode:
# "report" notes what it would do and never touches production, and "auto"
# lets Flightdeck roll production back by itself when every check passes.
resource "flightdeck_project" "payments" {
  name       = "Payments"
  identifier = "PAY"

  self_healing = {
    feature_enabled        = true
    rollback               = "report"
    bake_minutes           = 30
    burn_rate              = 10.0
    max_rollbacks_per_hour = 2
  }
}

# A project's Slack channel (project admins). Provisioning is asynchronous:
# enabling the channel enqueues the job, and `channel_id` and
# `provision_status` are filled in once it has run.
resource "flightdeck_project" "support" {
  name       = "Support"
  identifier = "SUP"

  slack_channel = {
    enabled = true
    name    = "team-support" # omit to use the project-name default

    # Only the categories listed here are managed; the API merges them, so
    # removing one leaves it as it is rather than restoring its default.
    event_filter = {
      logged        = true
      field_changed = false
    }
  }
}

# Agent work (workspace owners and admins only) lets AutoPilot agents take the
# work items a person marks with the agent label, and open pull requests for
# them. A flightdeck_label in the project depends on the project, so pointing
# label_id at one would be a dependency cycle; for a project that already
# exists, the label takes its project_id from a data source instead.
data "flightdeck_project" "billing" {
  identifier = "BILL"
}

resource "flightdeck_label" "agent_ready" {
  project_id = data.flightdeck_project.billing.id
  name       = "agent-ready"
}

# The service account agents work as. It needs a project role that can edit
# work items, because a claimed item is assigned to it.
data "flightdeck_workspace_member" "agent" {
  email = "agent-bot@example.com"
}

resource "flightdeck_project_member" "agent" {
  project_id = data.flightdeck_project.billing.id
  user_id    = data.flightdeck_workspace_member.agent.id
  role       = "member"
}

resource "flightdeck_project" "billing" {
  name       = "Billing"
  identifier = "BILL"

  agent_work = {
    enabled          = true
    kinds            = ["implement-work-item"]
    label_id         = flightdeck_label.agent_ready.id
    agent_account_id = data.flightdeck_workspace_member.agent.id
    max_in_progress  = 1
    daily_budget_usd = 10
    task_max_usd     = 2.5 # whole cents: more decimal places fail the plan
  }
}
