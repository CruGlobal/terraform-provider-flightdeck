package provider

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
)

func regexMust(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// randIdentifier returns a project identifier that satisfies the model's
// /\A[A-Z][A-Z0-9]{0,9}\z/ rule and is unique enough for a shared workspace.
func randIdentifier() string {
	return "T" + strings.ToUpper(acctest.RandStringFromCharSet(7, "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"))
}

func randName(prefix string) string {
	return fmt.Sprintf("%s %s", prefix, strings.ToLower(acctest.RandString(6)))
}
