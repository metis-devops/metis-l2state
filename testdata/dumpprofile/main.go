// dumpprofile profiles one raw export from an already prepared fixture.
// It is a diagnostic consumer, not an l2state CLI command. Verify its artifact
// independently after this process exits; never include verification in profiles.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
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
	mode := flag.String("mode", "cpu", "cpu, locks, or alloc")
	workers := flag.Int("workers", 8, "worker limit")
	codec := flag.String("compression", "zstd", "artifact compression")
	flag.Parse()
	if *source == "" || *out == "" || *dir == "" {
		return errors.New("source, out and profiles are required")
	}
	if *mode != "cpu" && *mode != "locks" && *mode != "alloc" {
		return errors.New("invalid profile mode")
	}
	if err := os.Mkdir(*dir, 0700); err != nil {
		return err
	}
	stop, err := startProfile(*mode, *dir)
	if err != nil {
		return err
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	result, dumpErr := migration.Dump(context.Background(), migration.DumpOptions{
		Source: *source, Output: *out, Workers: *workers, CacheMB: 64, Handles: 64, Compression: *codec,
	})
	runtime.ReadMemStats(&after)
	if err := errors.Join(dumpErr, stop()); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
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

func startProfile(mode, dir string) (func() error, error) {
	switch mode {
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
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		return func() error {
			runtime.SetMutexProfileFraction(0)
			runtime.SetBlockProfileRate(0)
			return errors.Join(saveProfile(dir, "mutex"), saveProfile(dir, "block"))
		}, nil
	default:
		runtime.MemProfileRate = 4096
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
