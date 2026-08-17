package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// errCancelled is what a closed stdin or a refused summary comes back as. It is
// not a failure and the caller does not print it as one.
var errCancelled = errors.New("setup: cancelled")

// ui is the whole of the interaction: questions on one side, a transcript on
// the other. It is a struct rather than a package of functions so the tests can
// drive a whole session from a script of answers.
type ui struct {
	in  *bufio.Reader
	out io.Writer
}

func newUI(in io.Reader, out io.Writer) *ui {
	return &ui{in: bufio.NewReader(in), out: out}
}

// printf is the one place anything is written. The error is dropped on purpose:
// a transcript that cannot be printed is not a reason to abandon a setup, and
// there is nowhere better to report it to than the thing that just failed.
func (u *ui) printf(format string, a ...any) {
	_, _ = fmt.Fprintf(u.out, format, a...)
}

func (u *ui) say(format string, a ...any) {
	u.printf(format+"\n", a...)
}

func (u *ui) blank() { u.printf("\n") }

// heading starts a section. Sections are numbered by the caller rather than
// counted here, because the number of questions depends on the answers.
func (u *ui) heading(title string) {
	u.blank()
	u.say("%s", title)
	u.say("%s", strings.Repeat("─", len([]rune(title))))
}

// note is the explanation under a question: what the choice means, in the
// second person, indented so it reads as commentary rather than as more
// questions.
func (u *ui) note(lines ...string) {
	for _, l := range lines {
		if l == "" {
			u.blank()
			continue
		}
		u.say("  %s", l)
	}
}

// good, warn and bad are the results of the checks setup runs as it goes. They
// are the reason this is a program rather than a page of documentation: it can
// look.
func (u *ui) good(format string, a ...any) { u.say("  ✓ "+format, a...) }
func (u *ui) warn(format string, a ...any) { u.say("  ! "+format, a...) }
func (u *ui) bad(format string, a ...any)  { u.say("  ✗ "+format, a...) }

// readLine returns the next answer. A closed input is a cancellation rather
// than an error: it is what Ctrl-D means.
func (u *ui) readLine() (string, error) {
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", errCancelled
	}
	return strings.TrimSpace(line), nil
}

// ask puts a question with a default. Empty takes the default; the default is
// shown so nobody has to guess what empty means.
func (u *ui) ask(question, def string) (string, error) {
	for {
		if def != "" {
			u.printf("  %s [%s]: ", question, def)
		} else {
			u.printf("  %s: ", question)
		}
		answer, err := u.readLine()
		if err != nil {
			return "", err
		}
		if answer == "" {
			answer = def
		}
		if answer != "" {
			return answer, nil
		}
		u.bad("an answer is needed here")
	}
}

// askOptional puts a question that may be left unanswered. Empty means "not
// now", which some questions genuinely have as an answer — a path that does not
// exist yet, an email nobody wants to give.
func (u *ui) askOptional(question string) (string, error) {
	u.printf("  %s [skip]: ", question)
	return u.readLine()
}

// yes asks a yes/no question.
func (u *ui) yes(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		u.printf("  %s [%s]: ", question, hint)
		answer, err := u.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		u.bad("answer y or n")
	}
}

// choice is one option, with the sentence that says what picking it means and
// an optional note from a check setup has just run.
type choice struct {
	label  string
	detail string
	// found is what a probe discovered about this option — "port 80 is free",
	// "nothing is listening there". Printed after the detail, because the
	// difference between the options is usually what this machine can do rather
	// than what the operator prefers.
	found string
}

// choose puts a numbered list. def is a 1-based index.
func (u *ui) choose(question string, choices []choice, def int) (int, error) {
	u.say("  %s", question)
	for i, c := range choices {
		u.printf("    %d) %-28s %s\n", i+1, c.label, c.detail)
		if c.found != "" {
			u.printf("       %s\n", c.found)
		}
	}
	for {
		u.printf("  Choose [%d]: ", def)
		answer, err := u.readLine()
		if err != nil {
			return 0, err
		}
		if answer == "" {
			return def, nil
		}
		n, err := strconv.Atoi(answer)
		if err == nil && n >= 1 && n <= len(choices) {
			return n, nil
		}
		u.bad("pick a number between 1 and %d", len(choices))
	}
}
