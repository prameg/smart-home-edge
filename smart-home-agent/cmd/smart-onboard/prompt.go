package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// prompter reads guided input from a single buffered stream (stdin in
// production). Keeping the reader on a struct means every prompt shares one
// buffer, so no typed line is ever lost between reads.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out}
}

// stdinIsInteractive reports whether stdin is a terminal, so the CLI only offers
// guided prompts when a human is actually there to answer them (piped/CI input
// falls through to flags + defaults instead of blocking on a read).
func stdinIsInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// readLine reads one trimmed line; io.EOF returns an empty string with no error
// so callers treat "pressed enter at EOF" as "accept the default".
func (p *prompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil {
		if err == io.EOF {
			return line, nil
		}

		return "", fmt.Errorf("read input: %w", err)
	}

	return line, nil
}

// text prompts with a default shown in brackets; an empty answer keeps def.
func (p *prompter) text(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}

	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}

	return line, nil
}

// required loops until a non-empty value is entered (no default is acceptable).
func (p *prompter) required(label string) (string, error) {
	for {
		fmt.Fprintf(p.out, "%s: ", label)
		line, err := p.readLine()
		if err != nil {
			return "", err
		}
		if line != "" {
			return line, nil
		}
		fmt.Fprintln(p.out, "  (required)")
	}
}

func (p *prompter) intVal(label string, def int) (int, error) {
	for {
		raw, err := p.text(label, strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(raw))
		if convErr != nil {
			fmt.Fprintf(p.out, "  (enter a number)\n")

			continue
		}

		return n, nil
	}
}

func (p *prompter) boolVal(label string, def bool) (bool, error) {
	raw, err := p.text(label+" (y/n)", yesNo(def))
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "y", "yes", "true":
		return true, nil
	case "n", "no", "false":
		return false, nil
	default:
		return def, nil
	}
}

// confirm asks a yes/no question, returning def on an empty answer.
func (p *prompter) confirm(label string, def bool) (bool, error) {
	return p.boolVal(label, def)
}

// pick shows a numbered menu and returns the chosen index, or -1 for the
// trailing "enter manually" escape hatch. An empty answer selects the default
// (index 0).
func (p *prompter) pick(label string, options []string) (int, error) {
	fmt.Fprintln(p.out, label)
	for i, opt := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, opt)
	}
	fmt.Fprintf(p.out, "  %d) enter a URL manually\n", len(options)+1)

	for {
		fmt.Fprintf(p.out, "Choice [1]: ")
		raw, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if raw == "" {
			return 0, nil
		}

		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > len(options)+1 {
			fmt.Fprintf(p.out, "  (choose 1-%d)\n", len(options)+1)

			continue
		}
		if n == len(options)+1 {
			return -1, nil
		}

		return n - 1, nil
	}
}

func yesNo(b bool) string {
	if b {
		return "y"
	}

	return "n"
}
