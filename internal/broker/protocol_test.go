package broker

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inkeliz/poudrine/internal/builder"
)

func TestWebBuildText(t *testing.T) {
	if got := builder.BuildText("\x1b[31mbuild\x1b[0m\t\x1b]8;;https://example.invalid\x07port\x1b]8;;\x1b\\\xff\x00"); got != "build port?" {
		t.Fatalf("terminal text: %q", got)
	}
	// Exercise the wire size with long, heavily escaped output and 16 jobs.
	b := &builder.Builder{LastWrite: time.Now().Add(time.Hour)}
	out := &builder.BuildOutput{B: b}
	line := strings.Repeat("\"\\\x01", 1024) + "\n"
	for range 40 {
		out.Write([]byte(line))
	}
	s := b.Status()
	for range 16 {
		s.Jobs = append(s.Jobs, builder.BuildJob{Package: strings.Repeat("a", 1024)})
	}
	frame, err := os.CreateTemp(t.TempDir(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	defer frame.Close()
	if err := writeConfigFrame(frame, Reply{Build: &s, Status: 200}); err != nil {
		t.Fatal(err)
	}
}
