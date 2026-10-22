package ovmevents

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestProgressDuringIncompleteBatch(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[canceled], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := fakeChain()
				o := syncOptions(t, 3)
				o.BatchSize = 4
				var progress bytes.Buffer
				o.Progress = &progress
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				inspected := false
				b.onHeader = func(tag string) error {
					if tag == "0x1" || tag == "0x2" {
						time.Sleep(3 * time.Second)
					}
					if tag == "0x3" && !inspected {
						inspected = true
						// Still inside the first batch, before any event query.
						text := progress.String()
						if !strings.Contains(text, "checked=3 headers=4") || strings.Contains(text, "Synchronized OVM events") {
							t.Fatal("missing or misleading in-flight progress:", text)
						}
						if len(b.requests) != 0 {
							t.Fatal("queried events before validating headers")
						}
						if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("incomplete batch was published:", err)
						}
						if canceled {
							cancel()
							return ctx.Err()
						}
					}
					return nil
				}
				_, err := synchronize(ctx, o, b, Save)
				if canceled {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("canceled batch was published:", err)
					}
					return
				}
				check(t, err)
				for _, message := range []string{"Checking RPC chain identity", "RPC chain identified", "Fetching OVM event logs", "Validating OVM event logs", "Synchronized OVM events", "Persisted OVM events"} {
					if !strings.Contains(progress.String(), message) {
						t.Fatal("missing phase:", message)
					}
				}
				d, _, err := ReadFile(t.Context(), o.Output)
				check(t, err)
				if d.Progress.BlockNumber != 3 {
					t.Fatal("wrong completed checkpoint", d.Progress)
				}
			})
		})
	}
}

func TestProgressWhenFollowingHead(t *testing.T) {
	b := fakeChain()
	b.head = 0
	o := syncOptions(t, 0)
	o.ToBlock = nil
	var progress bytes.Buffer
	o.Progress = &progress
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	polls := 0
	b.onHeader = func(tag string) error {
		if tag == "latest" {
			polls++
			if polls == 4 {
				cancel()
				return ctx.Err()
			}
		}
		return nil
	}
	_, err := synchronize(ctx, o, b, Save)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if strings.Count(progress.String(), "Waiting for new RPC blocks") != 1 {
		t.Fatal("waiting must be visible without repeating each poll:", progress.String())
	}
}
