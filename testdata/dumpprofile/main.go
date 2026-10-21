// dumpprofile profiles one dump or migration from an already prepared fixture.
// It is a diagnostic consumer, not an l2state CLI command. Verify its artifact
// independently after this process exits; never include verification in profiles.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"

	"github.com/metis-devops/metis-l2state/internal/migration"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	source := flag.String("source", "", "stopped fixture source")
	out := flag.String("out", "", "new artifact path")
	dir := flag.String("profiles", "", "new profile directory")
	mode := flag.String("mode", "cpu", "cpu, locks, alloc, or all")
	sampleBytes := flag.Int("allocation-sample-bytes", 4096, "allocation profile sample interval")
	blockRate := flag.Int("block-rate-ns", 1, "block profile sample interval")
	mutexFraction := flag.Int("mutex-fraction", 1, "mutex profile sample fraction")
	cache := flag.Int("cache-mb", 64, "source LevelDB cache MiB")
	handles := flag.Int("handles", 64, "source LevelDB file handles")
	workers := flag.Int("workers", 8, "worker limit")
	command := flag.String("command", "dump", "dump or migrate")
	events := flag.String("ovm-events", "", "RPC checkpoint for migrate")
	audit := flag.String("audit", "", "optional migration audit")
	runtimeCode := flag.String("runtime", "", "wrapped runtime for migrate")
	codec := flag.String("compression", "zstd", "artifact compression")
	flag.Parse()
	if *source == "" || *out == "" || *dir == "" {
		return errors.New("source, out and profiles are required")
	}
	if *mode != "cpu" && *mode != "locks" && *mode != "alloc" && *mode != "all" {
		return errors.New("invalid profile mode")
	}
	if err := os.Mkdir(*dir, 0700); err != nil {
		return err
	}
	stop, err := startProfile(*mode, *dir, *sampleBytes, *blockRate, *mutexFraction)
	if err != nil {
		return err
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var result migration.Result
	var dumpErr error
	switch *command {
	case "dump":
		result, dumpErr = migration.Dump(ctx, migration.DumpOptions{
			Source: *source, Output: *out, Workers: *workers, CacheMB: *cache, Handles: *handles, Compression: *codec,
		})
	case "migrate":
		result, dumpErr = migration.Migrate(ctx, migration.Options{
			Source: *source, Output: *out, Workers: *workers, CacheMB: *cache, Handles: *handles, Compression: *codec,
			OVMEvents: *events, AuditSummary: *audit, WrappedEtherCode: *runtimeCode, Progress: os.Stderr,
		})
	default:
		dumpErr = errors.New("invalid command")
	}
	runtime.ReadMemStats(&after)
	if err := errors.Join(dumpErr, stop()); err != nil {
		return err
	}
	// Keep this helper copyable into baseline checkouts without new dependencies.
	return json.MarshalEncode(jsontext.NewEncoder(os.Stdout), struct {
		Result         migration.Result
		AllocatedBytes uint64
		Mallocs        uint64
		GCs            uint32
		GCPauseNS      uint64
		HeapAfterBytes uint64
		MemProfileRate int
	}{result, after.TotalAlloc - before.TotalAlloc, after.Mallocs - before.Mallocs,
		after.NumGC - before.NumGC, after.PauseTotalNs - before.PauseTotalNs, after.HeapAlloc, runtime.MemProfileRate})
}

func startProfile(mode, dir string, sampleBytes, blockRate, mutexFraction int) (func() error, error) {
	switch mode {
	case "all":
		cpu, err := startProfile("cpu", dir, sampleBytes, blockRate, mutexFraction)
		if err != nil {
			return nil, err
		}
		locks, err := startProfile("locks", dir, sampleBytes, blockRate, mutexFraction)
		if err != nil {
			return nil, errors.Join(err, cpu())
		}
		alloc, err := startProfile("alloc", dir, sampleBytes, blockRate, mutexFraction)
		if err != nil {
			return nil, errors.Join(err, cpu(), locks())
		}
		return func() error { return errors.Join(cpu(), locks(), alloc()) }, nil
	case "cpu":
		f, err := os.Create(filepath.Join(dir, "cpu.pprof"))
		if err != nil {
			return nil, err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		return func() error { pprof.StopCPUProfile(); return f.Close() }, nil
	case "locks":
		runtime.SetMutexProfileFraction(mutexFraction)
		runtime.SetBlockProfileRate(blockRate)
		return func() error {
			runtime.SetMutexProfileFraction(0)
			runtime.SetBlockProfileRate(0)
			return errors.Join(saveProfile(dir, "mutex"), saveProfile(dir, "block"))
		}, nil
	default:
		runtime.MemProfileRate = sampleBytes
		return func() error {
			// Flush completed allocation samples. This GC is outside Dump and
			// outside the MemStats delta; profiled runs are never timed samples.
			runtime.GC()
			return errors.Join(saveProfile(dir, "allocs"), saveProfile(dir, "heap"))
		}, nil
	}
}

func saveProfile(dir, name string) error {
	f, err := os.Create(filepath.Join(dir, name+".pprof"))
	if err != nil {
		return err
	}
	return errors.Join(pprof.Lookup(name).WriteTo(f, 0), f.Close())
}
