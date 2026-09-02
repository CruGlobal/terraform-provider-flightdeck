package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const labelRes = "flightdeck_label.test"

func labelConfig(env *testEnv, identifier, body string) string {
	return projectFixture(env, identifier) + fmt.Sprintf(`
resource "flightdeck_label" "test" {
  project_id = flightdeck_project.parent.id
%s
}
`, body)
}

func TestLabel_basicLifecycle(t *testing.T) {
	env := newTestEnv(t, "label")
	identifier := randIdentifier()
	var id string
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: labelConfig(env, identifier, `  name = "Security"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(labelRes, "id"),
					resource.TestCheckResourceAttrPair(labelRes, "project_id", "flightdeck_project.parent", "id"),
					resource.TestCheckResourceAttr(labelRes, "name", "Security"),
					resource.TestCheckResourceAttrSet(labelRes, "color"),
					resource.TestCheckResourceAttrSet(labelRes, "lock_version"),
					captureAttr(labelRes, "id", &id),
				),
			},
			{
				ResourceName:      labelRes,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: labelConfig(env, identifier, `
  name  = "Security review"
  color = "#dc2626"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(labelRes, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(labelRes, "id", &id),
					resource.TestCheckResourceAttr(labelRes, "name", "Security review"),
					resource.TestCheckResourceAttr(labelRes, "color", "#dc2626"),
				),
			},
		},
	})
}

func TestLabel_validationAndDuplicates(t *testing.T) {
	env := newTestEnv(t, "label")
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				Config: labelConfig(env, identifier, `
  name  = "x"
  color = "blue"`),
				ExpectError: regexMust(`must be a hex color`),
			},
			{
				// "Bug" is a seeded starter label.
				Config:      labelConfig(env, identifier, `  name = "Bug"`),
				ExpectError: regexMust(`(?s)Error creating Flightdeck label.*HTTP 422 \(validation_failed\).*Name has already\s+been taken`),
			},
		},
	})
}

func TestLabel_updateSendsIfMatchAndCreateSendsKey(t *testing.T) {
	env := newTestEnv(t, "label")
	env.requireFake(t)
	identifier := randIdentifier()
	runTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{Config: labelConfig(env, identifier, `  name = "v1"`)},
			{Config: labelConfig(env, identifier, `  name = "v2"`)},
		},
	})
	posts := env.fake.RequestsMatching("POST", "/api/v1/projects/")
	var labelPosts int
	for _, p := range posts {
		if len(p.Path) > len("/labels") && p.Path[len(p.Path)-len("/labels"):] == "/labels" {
			labelPosts++
			if p.Header.Get("Idempotency-Key") == "" {
				t.Error("label create sent no Idempotency-Key")
			}
		}
	}
	if labelPosts != 1 {
		t.Errorf("expected 1 label POST, got %d", labelPosts)
	}
	patches := env.fake.RequestsMatching("PATCH", "/api/v1/labels/")
	if len(patches) != 1 || patches[0].Header.Get("If-Match") != `"0"` {
		t.Errorf("label PATCHes = %+v", patches)
	}
}
