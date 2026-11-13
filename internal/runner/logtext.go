package runner

import (
	"bufio"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxLogLine caps a single stored log line. Longer lines are cut and marked,
// so one huge line (a minified bundle, a base64 blob) cannot fill the database.
const maxLogLine = 64 * 1024

var (
	// CSI sequences (colours, cursor moves, erase), OSC sequences (window
	// titles, hyperlinks) ended by BEL or ESC \, and any other two-byte escape.
	ansiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	// Control characters that mean nothing in a log view. Tab is kept.
	ctrlRe = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)
)

// cleanLine removes terminal escape codes and control characters from one line
// of agent output. A carriage return means "redraw this line", as progress bars
// do, so only the text after the last one is kept.
func cleanLine(s string) string {
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		if tail := s[i+1:]; strings.TrimSpace(tail) != "" {
			s = tail
		} else {
			s = strings.TrimRight(s[:i], "\r")
			if j := strings.LastIndexByte(s, '\r'); j >= 0 {
				s = s[j+1:]
			}
		}
	}
	s = ansiRe.ReplaceAllString(s, "")
	s = ctrlRe.ReplaceAllString(s, "")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// readLines calls emit for every line in r. Unlike bufio.Scanner it never
// stops on a long line: the excess is dropped and the line is marked, so the
// child process is never left blocked on a full pipe.
func readLines(r io.Reader, emit func(string)) {
	br := bufio.NewReaderSize(r, 32*1024)
	var line []byte
	truncated := false
	flush := func() {
		text := cleanLine(string(line))
		if truncated {
			text += " …[line cut]"
		}
		emit(text)
		line, truncated = line[:0], false
	}
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 {
			if room := maxLogLine - len(line); room > 0 {
				if len(chunk) > room {
					chunk, truncated = chunk[:room], true
				}
				line = append(line, chunk...)
			} else {
				truncated = true
			}
		}
		if err != nil {
			if len(line) > 0 || truncated {
				flush()
			}
			return
		}
		if !isPrefix {
			flush()
		}
	}
}
