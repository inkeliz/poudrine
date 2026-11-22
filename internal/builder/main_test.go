package builder

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// The worker launches its dedicated reaper using its own executable.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "poudriere-command" {
		if err := RunPoudriereCommand(context.Background(), os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Exercise the same helper startup/exit timing under go test -race.
	// The race runtime otherwise delays every test-executable child by 1s.
	os.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	os.Exit(m.Run())
}
