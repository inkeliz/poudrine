package poudrine

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// A cursor belongs to one package in one Poudriere phase. Only the current
// compiler target reports a percentage; the other phases have no known total.
type jobLogs map[string]jobLogCursor

type jobLogCursor struct {
	file    os.FileInfo
	offset  int64
	inBuild bool
	percent *int
}

const progressTail = 32 << 10

var phaseHeader = regexp.MustCompile(`^=+<phase: ([^ >]+) *>={3,}$`)
var compilerStep = regexp.MustCompile(`^\[ *([0-9]{1,9})(%|/([0-9]{1,9}))\](?:[ \t]|$)`)
var logTimePrefix = regexp.MustCompile(`^\[[0-9]+:[0-9]{2}:[0-9]{2}\][ \t]*`)

func (c *jobLogCursor) progress(dir *os.Root, pkg string) *int {
	f, err := dir.OpenFile(filepath.Join("logs", pkg+".log"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	if c.file == nil || !os.SameFile(c.file, info) || info.Size() < c.file.Size() ||
		info.Size() == c.file.Size() && info.ModTime() != c.file.ModTime() {
		*c = jobLogCursor{}
	}
	// Compilers can emit megabytes between polls. Read complete recent lines
	// only and keep the last measurement while a long command is silent.
	offset := max(c.offset, info.Size()-progressTail)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, info.Size()-offset))
	if err != nil {
		return nil
	}
	text := string(data)
	missedHeader := !c.inBuild && offset > c.offset
	if offset != c.offset {
		// A bounded read can begin halfway through a line.
		_, tail, _ := strings.Cut(text, "\n")
		offset += int64(len(text) - len(tail))
		text = tail
	}
	for {
		line, tail, complete := strings.Cut(text, "\n")
		if !complete {
			break
		}
		offset += int64(len(line) + 1)
		text = tail
		line = strings.TrimSuffix(line, "\r")
		line = line[len(logTimePrefix.FindString(line)):]
		if phase := phaseHeader.FindStringSubmatch(line); phase != nil {
			missedHeader = false
			c.inBuild = phase[1] == "build"
			c.percent = nil
		} else if c.inBuild {
			if percent, ok := compilePercent(line); ok {
				c.percent = new(percent)
			}
		}
	}
	// If our first view was too late for the build header, the existing
	// tail could belong to configure. Trust only future appended output.
	if missedHeader {
		c.inBuild = true
	}
	c.offset, c.file = offset, info
	return c.percent
}

func compilePercent(line string) (int, bool) {
	m := compilerStep.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	done, _ := strconv.ParseInt(m[1], 10, 64)
	total := int64(100)
	if m[3] != "" {
		total, _ = strconv.ParseInt(m[3], 10, 64)
	}
	if done > total || total == 0 {
		return 0, false
	}
	return int(100 * done / total), true
}
