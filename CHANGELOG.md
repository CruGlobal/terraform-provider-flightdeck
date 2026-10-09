# Changelog

## [0.8.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.7.0...v0.8.0) (2026-10-09)


### ⚠ BREAKING CHANGES

* flightdeck_project now sends terraform_managed on every create and update. Creating any project fails against a Flightdeck without terraform_managed (FD-1171), even with terraform_managed = false. Once Flightdeck has it, every existing project plans terraform_managed false -> true, and applying that needs a token whose user is a workspace owner or admin; set terraform_managed = false or add lifecycle { ignore_changes = [terraform_managed] } to leave a project's flag off.

### Added

* mark the projects flightdeck_project manages as terraform_managed (needs Flightdeck's terraform_managed and a workspace owner or admin token) ([#40](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/40)) ([04c796b](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/04c796b7db399a255b4b199fbff3b9e3e793f653))


### Fixed

* build with Go 1.26.9 and golang.org/x/net v0.60.0 to clear GO-2026-6611 through GO-2026-6617 ([#42](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/42)) ([76b697e](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/76b697e1e9c3cc05cac60c6b7c5bcff3b4375a80))

## [0.7.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.6.0...v0.7.0) (2026-10-07)


### ⚠ BREAKING CHANGES

* never send a body on a read, require a count on threshold error rules, and explain lost races and 404s ([#34](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/34))

### Added

* manage a project's agent work settings (flightdeck_project.agent_work) ([#33](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/33)) ([c66955b](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/c66955b1ab70f17f5544e999d262e4f989838bf1))
* manage teamspaces, their members and the projects they own (needs Flightdeck's teamspaces API) ([#35](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/35)) ([c0c7380](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/c0c7380f7ea18dc52d13a270a349269a1ddfa9a7))
* set condition.count_browser_errors on error alert rules ([#30](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/30)) ([4fe2ce9](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/4fe2ce926507cf68a02e193e765538205b3a0ae5))


### Fixed

* never send a body on a read, require a count on threshold error rules, and explain lost races and 404s ([#34](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/34)) ([60391d5](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/60391d50cecf4128f22a04e6a2b0a30091afd267))
* recover a lost create response, never revoke a live secret on replay, and warn when Slack needs an invite ([#32](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/32)) ([c53a3ac](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/c53a3ac029b29681062d00ec83d521a245a3ca81))
* refuse label, state and repository names longer than Flightdeck stores, at plan time (needs Flightdeck's name limits) ([#36](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/36)) ([d47c867](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/d47c8676ce9c3eb256f20066a590130163792ea4))
* report a refused lead, member or webhook project id against its attribute, and refuse ids below 1 at plan time (the clearer errors need Flightdeck's newer id checks) ([#38](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/38)) ([0919263](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/091926310071dfe69f80de53ef6d733fa894d057))
* retry a request that times out, when it is safe to send again ([#37](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/37)) ([5398d04](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/5398d042c3bad81c0acd4227e86a0ccd4225b8eb))
* warn when a resource leaves state because its project is gone or the token can no longer see it ([#39](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/39)) ([6da1c04](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/6da1c04ed1b005f108f9d2aa43c0eff1238f9e50))

## [0.6.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.5.0...v0.6.0) (2026-09-28)


### Added

* manage the deployed app a project belongs to (flightdeck_project.app) ([#26](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/26)) ([b575e1a](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/b575e1aad3d52e72c47a6e1e1edd546f89c597d5))
* set auto-rollback with self_healing.rollback ("report" | "auto") ([#27](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/27)) ([bab00f9](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/bab00f9712f908002a60395a4c5a801d33980f38))


### Fixed

* manage the epics feature and webhook events, which replaced modules ([#28](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/28)) ([52c54af](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/52c54aff4d617a0d83d36470eb72f953b2725781))

## [0.5.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.4.0...v0.5.0) (2026-09-23)


### Added

* report a Slack channel Flightdeck cannot post to, and warn when provisioning fails ([#24](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/24)) ([33317c0](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/33317c0037a657b8780ba33de8a3935cf88dbe35))

## [0.4.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.3.0...v0.4.0) (2026-09-18)


### Added

* expose ci_failure_action on flightdeck_github_integration ([#20](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/20)) ([5db6aac](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/5db6aac24f790ba09bf2fb77af7290eef7083539))
* incident alert rules, Events API routing keys and a per-project PagerDuty link ([#23](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/23)) ([68bd8b7](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/68bd8b7cd45f1e6fd4fd56111c952e339f662ad7))

## [0.3.0](https://github.com/CruGlobal/terraform-provider-flightdeck/compare/v0.2.0...v0.3.0) (2026-09-10)


### Added

* manage a project's Slack channel ([#16](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/16)) ([33833a3](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/33833a3f784c1f986622d88c03377f6afc659e5d))
* resolve workspace members by email ([#17](https://github.com/CruGlobal/terraform-provider-flightdeck/issues/17)) ([b0e0c38](https://github.com/CruGlobal/terraform-provider-flightdeck/commit/b0e0c3845e05c2049089fee246c6216a62077de0))

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
