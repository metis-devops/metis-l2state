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
	"runtime"
	"syscall"

	"github.com/metis-devops/metis-l2state/internal/migration"
	"github.com/metis-devops/metis-l2state/internal/version"
)

const usage = `l2state migrate --source-chaindata PATH --out FILE --wrapped-ether-code FILE --ovm-events FILE [options]
l2state dump --source-chaindata PATH --out FILE [options]
l2state version
l2state help

migrate converts OVM state; dump preserves original state without OVM inputs.
The source must be stopped or a consistent snapshot.
Output preserves the source header; finalStateRoot is recorded separately.
Storage keys are keccak256(slot). No ancient database is opened.
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "l2state:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := io.WriteString(stdout, usage)
		return err
	case "version":
		if len(args) != 1 {
			return errors.New("version accepts no arguments")
		}
		return json.NewEncoder(stdout).Encode(struct{ Tool, Version, Geth string }{version.ToolName, version.ToolVersion, version.GethVersion})
	case "dump":
		return dumpCommand(ctx, args[1:], stdout, stderr)
	case "migrate":
		return migrateCommand(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func migrateCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("migrate", flag.ContinueOnError)
	f.SetOutput(stderr)
	var o migration.Options
	f.StringVar(&o.CodeCache, "code-cache", "memory", "retain source bytecode: memory or none")
	f.StringVar(&o.Source, "source-chaindata", "", "stopped l2geth hot LevelDB")
	f.StringVar(&o.OVMEvents, "ovm-events", "", "RPC event JSON bound to source LastBlock (required)")
	f.StringVar(&o.Output, "out", "", "new binary state file")
	f.StringVar(&o.WrappedEtherCode, "wrapped-ether-code", "", "storage-compatible wrappedEther runtime hex file (required)")
	f.StringVar(&o.RetainList, "ovm-erc20-retain-list", "", "optional source-contract retain list")
	f.StringVar(&o.Alloc, "ovm-genesis-alloc", "", "optional post-conversion GenesisAlloc overlay")
	f.StringVar(&o.AuditSummary, "ovm-audit-summary", "", "optional mvm v1 audit; canonical evidence must exist in hot DB")
	quiet := commonFlags(f, &o.Compression, &o.Workers, &o.CacheMB, &o.Handles)
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
	result, err := migration.Migrate(ctx, o)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}

func commonFlags(f *flag.FlagSet, compression *string, workers, cache, handles *int) *bool {
	f.StringVar(compression, "compression", "zstd", "zstd or none")
	f.IntVar(workers, "workers", max(2, min(runtime.NumCPU(), 16)), "global workers, 2..16")
	f.IntVar(cache, "cache-mb", 512, "source LevelDB cache MiB (not an RSS limit)")
	f.IntVar(handles, "handles", 256, "source LevelDB open-file allowance")
	return f.Bool("quiet", false, "disable progress on stderr")
}

func dumpCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("dump", flag.ContinueOnError)
	f.SetOutput(stderr)
	var o migration.DumpOptions
	f.StringVar(&o.CodeCache, "code-cache", "none", "retain source bytecode: memory or none")
	f.StringVar(&o.Source, "source-chaindata", "", "stopped l2geth hot LevelDB")
	f.StringVar(&o.Output, "out", "", "new binary state file")
	quiet := commonFlags(f, &o.Compression, &o.Workers, &o.CacheMB, &o.Handles)
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
	result, err := migration.Dump(ctx, o)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
