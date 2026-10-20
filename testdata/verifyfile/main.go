// verifyfile is a bounded benchmark/test consumer, not an l2state CLI command.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/metis-devops/metis-l2state/internal/strictio"
	"github.com/metis-devops/metis-l2state/statefile"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("expected state file")
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		return err
	}
	summary, verifyErr := statefile.Verify(context.Background(), f)
	if err := errors.Join(verifyErr, f.Close()); err != nil {
		return err
	}
	return strictio.WriteJSON(os.Stdout, summary)
}
