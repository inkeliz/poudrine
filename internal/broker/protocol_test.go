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
	// Exercise the wire size with a full tail, saved rebuild reasons and a full chunk of jobs.
	b := &builder.Builder{LastWrite: time.Now().Add(time.Hour)}
	out := &builder.BuildOutput{B: b}
	line := strings.Repeat("\"\\", 512) + "\n"
	for range 40 {
		out.Write([]byte(line))
	}
	s := b.Status()
	for range 8 {
		s.RebuildReasons = append(s.RebuildReasons, "Deleting test.pkg: "+strings.Repeat("\"", 1000))
	}
	for range 24 {
		s.Jobs = append(s.Jobs, builder.BuildJob{Package: strings.Repeat("a", 1024)})
	}
	frame, err := os.CreateTemp(t.TempDir(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	defer frame.Close()
	if err := writeConfigReply(frame, Reply{Build: &s, Status: 200}); err != nil {
		t.Fatal(err)
	}
}
