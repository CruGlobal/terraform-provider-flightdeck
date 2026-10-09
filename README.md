# Terraform Provider Flightdeck

> **Status: AI-supported, not actively maintained.** Built for an
> internal use case at Cru. Dependabot keeps dependencies and security
> advisories current automatically (patch and minor bumps auto-merge;
> majors require manual review). Feature work and bug fixes happen on
> a best-effort basis. **Pull requests and issues are welcome** — they
> may take time to be reviewed.

`terraform-provider-flightdeck` manages a Flightdeck workspace's
**project configuration** through its REST API. Flightdeck is a
project-management application (workspaces, projects, work items) with
built-in error tracking and incident management; this provider covers
the settings that
define how an application's project is set up — the project itself, its
workflow states and labels, who has access, error-ingestion tokens,
error and incident alert rules, the Events API routing keys external
monitors use to open incidents, where incidents page, and outbound
webhooks — can live in Terraform next to the rest of that application's
infrastructure. It also manages teamspaces, the teams that say who owns
a project, with their members and the projects they own.

It deliberately does **not** manage runtime planning data (work items,
sprints, epics, comments). Those belong to the people using the
project, not to infrastructure code.

## Resources and data sources

| Resource | Manages |
| --- | --- |
| `flightdeck_project` | A project: name, identifier, description, emoji, archived flag, lead, visibility, feature toggles, self-healing configuration, Slack channel configuration, agent work settings, and whether Terraform manages it (`terraform_managed`, which makes the name, identifier, description, emoji, lead, visibility, deployed app, feature toggles, Slack channel and agent work settings read only in Flightdeck's app and turns off archive and restore there; self-healing stays editable); reports the (read-only) GitHub repository link. |
| `flightdeck_state` | A workflow state within a project (name, group, color, default, position). |
| `flightdeck_label` | A label within a project. |
| `flightdeck_project_member` | A user's membership of a project (by membership id) and their role. |
| `flightdeck_ingestion_token` | An error-ingestion token for a project (the secret is returned once, on create). |
| `flightdeck_error_alert_rule` | A trigger → conditions → action error alert rule. |
| `flightdeck_incident_alert_rule` | The incident-lifecycle sibling: a trigger → conditions → action rule on `incident_opened` or `incident_repeated`, with the priority it gives what it files. |
| `flightdeck_routing_key` | An Events API routing key: the credential an external monitor presents to open a Flightdeck incident. Independent of paging. |
| `flightdeck_pagerduty_integration` | A project's link to a PagerDuty service, forwarding signals to PagerDuty's Events API v2. One per project; the credential is a write-only argument. |
| `flightdeck_webhook` | An outbound webhook, workspace-wide or scoped to one project. |
| `flightdeck_github_integration` | A project's link to a GitHub repository, with Flightdeck registering the repository webhook or the caller supplying the shared secret, and what a failed workflow run files. |
| `flightdeck_teamspace` | A teamspace: a team in the workspace (name, description, lead). Names are not unique. |
| `flightdeck_teamspace_member` | A workspace member's place on a teamspace. |
| `flightdeck_teamspace_project` | A teamspace's ownership of a project. A project can belong to several teams. |

| Data source | Resolves |
| --- | --- |
| `flightdeck_project` | A project by id or identifier. |
| `flightdeck_states` | All workflow states of a project. |
| `flightdeck_workspace_member` | Resolves a workspace member by email address to a numeric user id, for `project_member.user_id`, `project.lead_id`, `project.agent_work.agent_account_id`, `teamspace.lead_id` and `teamspace_member.user_id`. |
| `flightdeck_teamspace` | A teamspace by id, or by name. Names are not unique, so a lookup by name fails unless exactly one team has it. |

`flightdeck_project_member.user_id`, `flightdeck_project.lead_id` and
`flightdeck_project.agent_work.agent_account_id` take a numeric user id;
`flightdeck_workspace_member` resolves one from an email address, so a
configuration can name people (and service accounts) rather than ids. The
match is exact — case and surrounding whitespace are ignored, nothing
else — and an address that resolves to nobody fails the plan.

Resources are added in the order the corresponding Flightdeck API
endpoints ship; see [`CHANGELOG.md`](./CHANGELOG.md) for what a given
release includes.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.13
  (`flightdeck_pagerduty_integration.routing_key` is a write-only
  argument, which needs >= 1.11 — already below this floor)
- A Flightdeck deployment whose `/api/v1` includes the project
  configuration endpoints, and a personal access token for the
  workspace you want to manage
- [Go](https://golang.org/doc/install) >= 1.26 (only for building from
  source; the exact version is pinned in [`.tool-versions`](./.tool-versions))

## Using the provider

```hcl
terraform {
  required_providers {
    flightdeck = {
      source  = "CruGlobal/flightdeck"
      version = "~> 0.1"
    }
  }
}

provider "flightdeck" {
  endpoint = "https://flightdeck.example.com"
  # token = var.flightdeck_token   # or set FLIGHTDECK_TOKEN
}

resource "flightdeck_project" "app" {
  name       = "Mobile App"
  identifier = "APP"
  features = {
    intake = true
    errors = true
  }
}
```

### Provider attributes

| Attribute  | Description                                                                                          |
| ---------- | ---------------------------------------------------------------------------------------------------- |
| `endpoint` | Base URL of the Flightdeck deployment. `/api/v1` is appended. Falls back to `FLIGHTDECK_ENDPOINT`.   |
| `token`    | Personal access token (`fd_pat_…`), sent as a bearer token. Sensitive. Falls back to `FLIGHTDECK_TOKEN`. |

A personal access token is created in the Flightdeck UI under your
account's API tokens. It is bound to one workspace and acts as you, with
your project roles; to manage several workspaces, configure one aliased
provider per token.

Full reference docs (generated from the provider schema) live in
[`docs/`](./docs/) and on the
[Terraform Registry](https://registry.terraform.io/providers/CruGlobal/flightdeck/latest).

### How the provider talks to the API

- Every create carries an `Idempotency-Key` derived from the resource's
  identity, so a retried create replays the original instead of making a
  duplicate.
- Routing keys, ingestion tokens and webhooks return their secret only to
  the create that made them, so a replay cannot hand it back. When a create's
  response is lost and the retry meets the record it made, the provider
  revokes that record (nobody holds its secret) and creates another. When the
  record was made by something else, such as a second resource declared with
  the same values, Flightdeck refuses the replay and the apply fails naming
  the record, without revoking anything.
- Every update carries the resource's `lock_version` as an `If-Match`
  precondition. If something else changed the resource in between, the
  API answers 409 and the provider reports it instead of overwriting;
  re-run `terraform plan` to pick up the change.
- Rate limiting (HTTP 429) is handled with client-side backoff that
  honours `Retry-After`.
- Each attempt at a request has 60 seconds to be answered. One that times
  out, or whose connection drops, before its answer arrives is sent again
  with backoff when that is safe: a read, a delete, a create carrying its
  `Idempotency-Key`, a teamspace member or project link (Flightdeck answers
  a repeat with the existing link), or an update carrying `If-Match`. Any
  other write is not resent, because the first attempt may have taken
  effect.
- Deletes carry `If-Match` too. A delete is not an overwrite, so a stale
  version there is answered by re-reading once and deleting with the
  current version. A project delete carries no precondition; if it loses
  a race to a write that added something to the project while it was
  being deleted (a 409), it is sent once more.
- A 404 always means "gone from this token's point of view" (including
  ids in another workspace, anything in a project the token cannot see,
  and projects mid-teardown) and removes the resource from state. On
  destroy it counts as success: of two racing deletes, one is answered
  and the other gets the 404. That includes a project the token has lost
  access to; see below.
- Reads never carry a request body. Flightdeck refuses a `GET` with one.

### Projects Terraform manages

Every project `flightdeck_project` manages is marked `terraform_managed`
unless its configuration says `terraform_managed = false`. While the flag
is on, Flightdeck's web app shows the settings Terraform owns as read only
(name, identifier, description, emoji, lead, visibility, deployed app,
feature toggles, the Slack channel and agent work settings) and turns off
archiving and restoring the project, and its MCP tools refuse to change
them. A change made there would only be undone by the next apply.
Flightdeck's API, which the provider uses, still writes everything.

- **The token needs a workspace owner or admin** to set or change the
  flag, on a create too. Sending back the value Flightdeck already has is
  fine for any token, so a token that isn't an owner or admin can still
  update a project whose flag already matches. Otherwise use such a
  token, or set `terraform_managed = false`.
- **To leave one project's flag as it is**, for example with a token that
  isn't an owner or admin, add this to the project:

  ```hcl
  lifecycle {
    ignore_changes = [terraform_managed]
  }
  ```

  That only covers a project that already exists. A create always sends
  the flag, `true` unless the configuration says `false`.
- **To hand a project back to the app**, apply `terraform_managed = false`
  first, then remove it from Terraform. A `removed` block or
  `terraform state rm` on its own leaves the flag on, and the project
  stays read only in the app.
- **If you already removed it without that step**, bring it back with
  `terraform import` (or an `import` block), apply
  `terraform_managed = false`, and then remove it again.
- **Importing** a project that isn't marked yet plans an update that sets
  the flag.

#### Upgrading to a provider with `terraform_managed`

The flag needs a Flightdeck that supports it. To check, read any project
with the `flightdeck_project` data source: its `terraform_managed` is
null on a Flightdeck without it, and `true` or `false` on one with it.

Do it in this order:

1. Upgrade Flightdeck.
2. Make the token's user a workspace owner or admin, or mark the projects
   that should stay unmarked with `terraform_managed = false` or the
   `ignore_changes` above.
3. Run `terraform plan` and apply. The first plan after Flightdeck gains
   the flag updates every existing project in place: `terraform_managed`
   goes from `false` to `true`, and `lock_version` shows as known after
   apply. Nothing else about the projects changes.

Against a Flightdeck without the flag, projects that already exist keep
working while their configuration leaves `terraform_managed` unset: it
reads as null and isn't sent. But creating any project fails, even with
`terraform_managed = false`, because a create always sends the flag, and
so does setting it explicitly. The error asks you to upgrade Flightdeck.
If Flightdeck is rolled back below the flag, plan with refresh on (not
`-refresh=false`) so the provider sees that the flag is gone.

### When the token can no longer see a project

Flightdeck answers 404, never 403, for a project the token's user can't
see, and for everything in it. So if that user is removed from a private
project, or a project is made private without them, the provider can't
tell the project from a deleted one. **The project and its resources
disappear from state, and the next plan offers to create them again.**
The provider warns when a project leaves state this way, and when a
resource in a project does while its project doesn't read back either.

Don't apply that plan while the project still exists: creating the
resources again would fail (the hidden project still holds its
identifier) or make copies. Instead:

1. Restore the token user's access to the project.
2. Bring each resource back into state with `terraform import`. The
   warning names each one's import id, and the commands are below.
   Routing keys and ingestion tokens come back without their secret,
   which Flightdeck returns only once.
3. Run `terraform plan` and check it creates nothing. An imported
   project lists every feature toggle, so unless the configuration
   names them all, the plan also shows an in-place update to the
   project's `features`. That only stops Terraform tracking the keys
   the configuration leaves out.

### Importing existing resources

Projects import by numeric id or by identifier; states, labels, webhooks,
GitHub integrations and teamspaces by their own numeric id; a
teamspace's members and projects by `<teamspace_id>/<user_id>` and
`<teamspace_id>/<project_id>`; project members,
ingestion tokens, error alert rules, incident alert rules and routing
keys by `<project_id>/<id>` (members also by
`<project_id>/user:<user_id>`); the PagerDuty link by the project id
alone, since there is one per project:

```sh
terraform import flightdeck_project.app 42
terraform import flightdeck_project.app APP
terraform import flightdeck_state.done 17
terraform import flightdeck_project_member.deploy_bot 42/user:7
terraform import flightdeck_error_alert_rule.new_errors 42/12
terraform import flightdeck_incident_alert_rule.opened 42/13
terraform import flightdeck_pagerduty_integration.app 42
terraform import flightdeck_teamspace.platform 12
terraform import flightdeck_teamspace_project.app 12/42
```

Each resource's documentation page shows its import command.

## Building from source

```sh
git clone https://github.com/CruGlobal/terraform-provider-flightdeck
cd terraform-provider-flightdeck
go build ./...
```

Pre-built, GPG-signed binaries are produced by goreleaser on every
GitHub Release and published to the public Terraform Registry.

## Developing

Common workflows are defined in [`Taskfile.yaml`](./Taskfile.yaml):

```sh
task build       # compile the provider
task install     # install the binary into $GOBIN for use with dev_overrides
task test        # run the test suite against the in-process fake API
task generate    # regenerate docs from schema (needs terraform on PATH)
task testacc     # run the test suite against a live Flightdeck workspace
```

### Testing

The test suite drives the provider through real Terraform plans and
applies. By default it runs against an in-process fake of the Flightdeck
API (`internal/flightdecktest`) that encodes the API contract — auth,
pagination, the error envelope, `Idempotency-Key` replay, `If-Match`
preconditions, 429 throttling — so the whole provider is testable
without a deployment. The terraform CLI must be on `PATH`.

The same tests run against a live Flightdeck when `TF_ACC=1` and
`FLIGHTDECK_ENDPOINT` / `FLIGHTDECK_TOKEN` point at a **dedicated test
workspace** (they create and delete projects). The token's user must be a
**workspace owner or admin**, because every project the tests create is
marked `terraform_managed`, and that Flightdeck must support the flag.
The member tests also need
`FLIGHTDECK_ACC_MEMBER_USER_ID`, the numeric user id of another member of
that workspace — a user id rather than an email so the tests do not
depend on a particular address — and the managed-mode GitHub-link tests
run only when `FLIGHTDECK_ACC_GITHUB_REPO` names a repository the
workspace's GitHub App can reach.

```sh
export TF_ACC=1
export FLIGHTDECK_ENDPOINT=https://flightdeck.example.com
export FLIGHTDECK_TOKEN=fd_pat_...
export FLIGHTDECK_ACC_MEMBER_USER_ID=7
task testacc
```

### Testing a local build against real Terraform configs

Terraform's `dev_overrides` mechanism lets you point Terraform at a
locally-built provider binary instead of resolving the provider through
the registry.

1. Build and install:

   ```sh
   task install
   ```

   `task install` prints the exact `~/.terraformrc` snippet you need —
   the path is whatever `go env GOBIN` resolves to (or
   `$(go env GOPATH)/bin` if `GOBIN` is unset).

2. Add the printed block to `~/.terraformrc` (create the file if it
   doesn't exist):

   ```hcl
   provider_installation {
     dev_overrides {
       "CruGlobal/flightdeck" = "/Users/you/go/bin"
     }

     # Leaves all other providers using the normal registry flow.
     direct {}
   }
   ```

3. In your test config, **do not run `terraform init`** — `dev_overrides`
   are mutually exclusive with the lockfile. Run `terraform plan` /
   `terraform apply` directly. Terraform will print a warning that
   confirms the override is active:

   ```
   Warning: Provider development overrides are in effect
   ```

4. Iterate: `task install` after each code change to refresh the
   binary, then re-run `terraform plan`.

## License

BSD 3-Clause. See [`LICENSE`](./LICENSE).
