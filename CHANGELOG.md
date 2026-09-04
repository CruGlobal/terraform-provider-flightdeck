# Changelog

## [0.2.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.1.0...v0.2.0) (2026-09-04)


### ⚠ BREAKING CHANGES

* the `flightdeck_workspace_member` data source is removed, with no replacement: the API has no route to resolve a workspace member by email. `flightdeck_project_member.user_id` takes the member's numeric user id, visible in the workspace's member list. `flightdeck_project_member` is keyed by membership id. Import as `<project_id>/<membership_id>`, or `<project_id>/user:<user_id>` to look the membership up by user; existing state for this resource must be re-imported. `flightdeck_error_alert_rule` imports as `<project_id>/<rule_id>`. `flightdeck_project.github_repo_full_name` is read-only and cannot be set; manage the repository link with a `flightdeck_github_integration` resource, which sets it on link and clears it on unlink. `flightdeck_webhook.secret` cannot be set; Flightdeck generates the signing secret and returns it once on create.
* reconcile members, ingestion tokens, alert rules and webhooks to the merged Flightdeck API ([#11](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/11))
* reconcile projects, states, labels and self-healing to the merged Flightdeck API ([#10](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/10))

### Added

* add flightdeck_github_integration ([#12](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/12)) ([4209e21](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/4209e21db3cc592b9ff5750563c25cb8b835988c))


### Fixed

* reconcile members, ingestion tokens, alert rules and webhooks to the merged Flightdeck API ([#11](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/11)) ([377b4a2](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/377b4a21401e73e4fb4d4fb7158ca92b87523f27))
* reconcile projects, states, labels and self-healing to the merged Flightdeck API ([#10](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/10)) ([9706651](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/9706651d82d092a1b1feedfc4a90ee880958a9d3))
* server-generated webhook secrets, enabled on new GitHub links, live gates open, pre-release review fixes ([#15](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/15)) ([fd3cec1](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/fd3cec193121d4218010d24f99ae255804fa2cf0))


### Changed

* bump golang.org/x/crypto, golang.org/x/net and google.golang.org/grpc past their advisories ([#14](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/14)) ([6a390c4](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/6a390c465a5e28cb818b349c4334b220a331e939))

## [0.1.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.0.0...v0.1.0) (2026-09-02)


### Added

* add flightdeck_project resource and data source ([#2](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/2)) ([7c53838](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/7c53838143a37746eb5d109ed28262647d3706ee))
* add flightdeck_state and flightdeck_label resources and flightdeck_states data source ([#3](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/3)) ([6abf560](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/6abf56026f35a19c0260cd6c8c36112241c3b4e7))
* add project member, ingestion token, error alert rule and webhook resources ([#4](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/4)) ([1234f91](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/1234f91340326158e4d232de2b3e4665c0e9b24c))
* add the self_healing block to flightdeck_project ([#5](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/5)) ([56d7178](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/56d71785949e8474d292891cd9198c699e34b43c))
* scaffold provider, REST client, provider configuration and release pipeline ([#1](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/1)) ([ffb5ce3](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/ffb5ce328b59b70924b47f74ffed7dc2204423a1))


### Changed

* **deps:** Bump golang.org/x/crypto from 0.50.0 to 0.52.0 ([#7](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/7)) ([5cfaa8d](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/5cfaa8deb806ceb05d8deaa5932ad4cf70d0416d))
* **deps:** Bump google.golang.org/grpc from 1.79.3 to 1.83.1 ([#8](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/8)) ([9b4a812](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/9b4a81208b102832e5bd37441f704564dbdb93f4))

## Changelog

All notable changes to this project are recorded here by
[release-please](https://github.com/googleapis/release-please) from
Conventional Commit messages.
