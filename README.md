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
alert rules and outbound webhooks — can live in Terraform next to the
rest of that application's infrastructure.

It deliberately does **not** manage runtime planning data (work items,
sprints, modules, comments). Those belong to the people using the
project, not to infrastructure code.

## Resources and data sources

| Resource | Manages |
| --- | --- |
| `flightdeck_project` | A project: name, identifier, description, emoji, archived flag, lead, visibility, feature toggles, self-healing thresholds; reports the (read-only) GitHub repository link. |
| `flightdeck_state` | A workflow state within a project (name, group, color, default, position). |
| `flightdeck_label` | A label within a project. |
| `flightdeck_project_member` | A user's role on a project. |
| `flightdeck_ingestion_token` | An error-ingestion token for a project (the secret is returned once, on create). |
| `flightdeck_error_alert_rule` | A trigger → conditions → action error alert rule. |
| `flightdeck_webhook` | An outbound webhook, workspace-wide or scoped to one project. |

| Data source | Resolves |
| --- | --- |
| `flightdeck_project` | A project by id or identifier. |
| `flightdeck_workspace_member` | A workspace member by email, so configuration can name people rather than ids. |
| `flightdeck_states` | All workflow states of a project. |

Resources are added in the order the corresponding Flightdeck API
endpoints ship; see [`CHANGELOG.md`](./CHANGELOG.md) for what a given
release includes.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.13
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
- Every update carries the resource's `lock_version` as an `If-Match`
  precondition. If something else changed the resource in between, the
  API answers 409 and the provider reports it instead of overwriting;
  re-run `terraform plan` to pick up the change.
- Rate limiting (HTTP 429) is handled with client-side backoff that
  honours `Retry-After`.
- Deletes carry `If-Match` too. A delete is not an overwrite, so a stale
  version there is answered by re-reading once and deleting with the
  current version.
- A 404 always means "gone from this token's point of view" (including
  ids in another workspace and projects mid-teardown) and removes the
  resource from state.

### Importing existing resources

Every resource can be imported by its numeric id; projects can also be
imported by identifier:

```sh
terraform import flightdeck_project.app 42
terraform import flightdeck_project.app APP
```

Nested resources use their own id (`terraform import flightdeck_state.done 17`).
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
workspace** (they create and delete projects). Some tests also need
`FLIGHTDECK_ACC_MEMBER_EMAIL`, the email of another member of that
workspace.

```sh
export TF_ACC=1
export FLIGHTDECK_ENDPOINT=https://flightdeck.example.com
export FLIGHTDECK_TOKEN=fd_pat_...
export FLIGHTDECK_ACC_MEMBER_EMAIL=someone@example.com
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
