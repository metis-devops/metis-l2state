package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/internal/version"
)

const usage = `ovm-event-sync --rpc-url URL --out events.json [--to-block N] [options]
ovm-event-sync help
ovm-event-sync version

Rebuild OVM event indexes from genesis using a trusted historical log RPC.
Existing output resumes automatically. Reorgs require a new output file.
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ovm-event-sync:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	if args[0] == "version" {
		if len(args) != 1 {
			return errors.New("version accepts no arguments")
		}
		return json.NewEncoder(stdout).Encode(struct{ Tool, Version, Geth string }{"ovm-event-sync", version.ToolVersion, version.GethVersion})
	}
	f := flag.NewFlagSet("ovm-event-sync", flag.ContinueOnError)
	f.SetOutput(stderr)
	var o ovmevents.Options
	f.StringVar(&o.RPCURL, "rpc-url", "", "historical Ethereum JSON-RPC endpoint")
	f.StringVar(&o.Output, "out", "", "JSON checkpoint (created or resumed)")
	f.Uint64Var(&o.BatchSize, "batch-size", 1000, "maximum blocks per batch, 1..10000")
	f.DurationVar(&o.PollInterval, "poll-interval", 5*time.Second, "head polling interval")
	f.DurationVar(&o.FlushInterval, "flush-interval", 30*time.Second, "checkpoint refresh interval")
	f.DurationVar(&o.RPCTimeout, "rpc-timeout", 30*time.Second, "per-request timeout")
	f.Func("to-block", "inclusive target height; omitted follows head", func(v string) error {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return err
		}
		o.ToBlock = &n
		return nil
	})
	quiet := f.Bool("quiet", false, "disable stderr progress")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if !*quiet {
		o.Progress = stderr
	}
	result, err := ovmevents.Sync(ctx, o)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
