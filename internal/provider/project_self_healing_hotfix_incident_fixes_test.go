package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/CruGlobal/terraform-provider-flightdeck/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// A project the tests create has no linked repository and no incidents, so
// turning hotfix_incident_fixes on never builds anything, live or fake.

func TestProjectSelfHealing_hotfixIncidentFixesRoundTrips(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				// The server's default, read back before anything sets it.
				Config: projectConfig(env, identifier, `  name = "Hotfix builds"`),
				Check:  resource.TestCheckResourceAttr(projectRes, "self_healing.hotfix_incident_fixes", "false"),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Hotfix builds"
  self_healing = {
    hotfix_incident_fixes = true
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.hotfix_incident_fixes", "true"),
			},
			{
				// Unset keeps what the project has.
				Config: projectConfig(env, identifier, `
  name = "Hotfix builds"
  self_healing = {
    bake_minutes = 25
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "25"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.hotfix_incident_fixes", "true"),
				),
			},
			{
				// false is a value, sent and saved, not a blank.
				Config: projectConfig(env, identifier, `
  name = "Hotfix builds"
  self_healing = {
    hotfix_incident_fixes = false
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.hotfix_incident_fixes", "false"),
			},
		},
	})
}

// A value changed in the console is drift while Terraform owns the setting,
// and is simply read while it does not.
func TestProjectSelfHealing_hotfixIncidentFixesConsoleChange(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	var id string
	owned := projectConfig(env, identifier, `
  name = "Console hotfix"
  self_healing = {
    hotfix_incident_fixes = true
  }`)
	unowned := projectConfig(env, identifier, `
  name = "Console hotfix"
  self_healing = {
    bake_minutes = 20
  }`)
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: owned,
				Check:  captureAttr(projectRes, "id", &id),
			},
			{
				PreConfig: func() { env.fake.SetHotfixIncidentFixes(mustInt(id), false) },
				Config:    owned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(projectRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.hotfix_incident_fixes", "true"),
			},
			{
				PreConfig: func() { env.fake.SetHotfixIncidentFixes(mustInt(id), false) },
				Config:    unowned,
				Check:     resource.TestCheckResourceAttr(projectRes, "self_healing.hotfix_incident_fixes", "false"),
			},
			{
				Config: unowned,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// A configuration that never names hotfix_incident_fixes never sends it.
func TestProjectSelfHealing_hotfixIncidentFixesOmittedIsNotSent(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Untouched hotfix"
  self_healing = {
    rollback     = "report"
    bake_minutes = 33
  }`),
				Check: resource.TestCheckResourceAttr(projectRes, "self_healing.bake_minutes", "33"),
			},
		},
	})
	sent := false
	for _, r := range env.fake.RequestsMatching("PATCH", "/api/v1/projects/") {
		if !strings.HasSuffix(r.Path, "/self-healing") {
			continue
		}
		sent = true
		if strings.Contains(string(r.Body), "hotfix_incident_fixes") {
			t.Fatalf("self-healing write named hotfix_incident_fixes although the configuration did not: %s", r.Body)
		}
	}
	if !sent {
		t.Fatal("no self-healing write was made, so the test proves nothing")
	}
}

// Terraform refuses a value that isn't a bool before anything reaches
// Flightdeck, which would refuse it too.
func TestProjectSelfHealing_hotfixIncidentFixesNotABool(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Bad hotfix"
  self_healing = {
    hotfix_incident_fixes = "maybe"
  }`),
				ExpectError: regexMust(`(?s)hotfix_incident_fixes.*a bool is required`),
			},
		},
	})
}

// A Flightdeck from before hotfix_incident_fixes reads it as null, and setting
// it says the server is too old rather than showing the bare refusal.
func TestProjectSelfHealing_hotfixIncidentFixesOnAnOlderFlightdeck(t *testing.T) {
	env := newTestEnv(t, "self_healing")
	env.requireFake(t)
	env.fake.SetHotfixIncidentFixesSupported(false)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: projectConfig(env, identifier, `
  name = "Older hotfix"
  self_healing = {
    bake_minutes = 30
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectRes, "self_healing.hotfix_incident_fixes"),
					resource.TestCheckResourceAttr(projectRes, "self_healing.monitor_rollback", "off"),
				),
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Older hotfix"
  self_healing = {
    bake_minutes = 30
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: projectConfig(env, identifier, `
  name = "Older hotfix"
  self_healing = {
    hotfix_incident_fixes = true
  }`),
				ExpectError: regexMust(`This Flightdeck is too old for\s+self_healing\.hotfix_incident_fixes`),
			},
		},
	})
}

func TestSelfHealingToObject_hotfixIncidentFixes(t *testing.T) {
	ctx := context.Background()
	yes, no := true, false
	for _, tc := range []struct {
		name string
		sh   client.SelfHealing
		want types.Bool
	}{
		{name: "on", sh: client.SelfHealing{HotfixIncidentFixes: &yes}, want: types.BoolValue(true)},
		{name: "off", sh: client.SelfHealing{HotfixIncidentFixes: &no}, want: types.BoolValue(false)},
		{name: "older server", sh: client.SelfHealing{}, want: types.BoolNull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			obj := selfHealingToObject(&tc.sh, &diags)
			var m selfHealingModel
			diags.Append(obj.As(ctx, &m, objectAsOptions)...)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !m.HotfixIncidentFixes.Equal(tc.want) {
				t.Fatalf("hotfix_incident_fixes = %s, want %s", m.HotfixIncidentFixes, tc.want)
			}
		})
	}
}
