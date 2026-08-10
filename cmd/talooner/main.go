// Command talooner is the operator CLI: cluster login, repo onboarding, and
// running rulesets against a live PR without writing anything.
package main

import (
	"fmt"
	"os"

	"github.com/opentalon/talooner/internal/version"
)

func main() {
	fmt.Fprintf(os.Stderr, "talooner %s\n", version.Version)
}
