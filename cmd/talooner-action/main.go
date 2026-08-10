// Command talooner-action is the GitHub Action entry point. It runs once per
// workflow event, inside the reviewed repo's own runner, and exits.
package main

import (
	"fmt"
	"os"

	"github.com/opentalon/talooner/internal/version"
)

func main() {
	fmt.Fprintf(os.Stderr, "talooner-action %s\n", version.Version)
}
