package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// A command is one line of `repogo help`: its name, what follows it, what it
// does, and its own flags. Flags belong to their command; any other is refused.
type command struct {
	name    string
	args    string
	summary string
	// hidden keeps it out of the list; `repogo help <name>` still shows it.
	hidden bool
	// gap puts a blank line before it in the list.
	gap   bool
	flags func(*flag.FlagSet)
	run   func(ctx context.Context, fs *flag.FlagSet, args []string) error
}

// commands is `repogo help`, in its order; run in commands.go.
var commands []command

func init() { commands = table() }

func find(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// dispatch runs args[0] with the rest. Flags may come before or after the
// command's own words: `repogo devices revoke --all`.
func dispatch(ctx context.Context, args []string) error {
	name := ""
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-version":
			name, args = "version", args[1:]
		case "--help", "-help", "-h":
			name, args = "help", args[1:]
		default:
			if !strings.HasPrefix(args[0], "-") {
				name, args = args[0], args[1:]
			}
		}
	}
	c, ok := find(name)
	if !ok {
		return fmt.Errorf("unknown command %q; run repogo help", name)
	}
	fs := flag.NewFlagSet(commandName(name), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if c.flags != nil {
		c.flags(fs)
	}
	rest, err := parseAll(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		printCommand(os.Stdout, c)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w for %s; run repogo help %s", cleanFlagError(err), commandName(name), name)
	}
	return c.run(ctx, fs, rest)
}

// parseAll parses flags anywhere among args, returning the words between them.
func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return words, nil
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// cleanFlagError says "unknown flag --now" where the flag package says
// "flag provided but not defined: -now".
func cleanFlagError(err error) error {
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
		return fmt.Errorf("unknown flag -%s", name)
	}
	return err
}

func commandName(name string) string {
	if name == "" {
		return "repogo"
	}
	return "repogo " + name
}

// noArgs refuses words a command doesn't take.
func noArgs(name string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s takes no arguments, not %q; run repogo help %s", commandName(name), strings.Join(args, " "), name)
	}
	return nil
}

// printHelp is `repogo help`: one command per line, flags written --name.
func printHelp(w io.Writer) {
	fmt.Fprintln(w, "RepoGo host: run coding agents on this computer from your phone.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: repogo [command]")
	fmt.Fprintln(w)
	for _, c := range commands {
		if c.hidden {
			continue
		}
		if c.gap {
			fmt.Fprintln(w)
		}
		line := strings.TrimSpace(commandName(c.name) + " " + c.args)
		if len(line) <= 28 {
			fmt.Fprintf(w, "  %-28s %s\n", line, c.summary)
		} else {
			fmt.Fprintf(w, "  %s\n  %-28s %s\n", line, "", c.summary)
		}
	}
}

// printCommand is `repogo help <command>`: its line and each of its flags.
func printCommand(w io.Writer, c command) {
	fmt.Fprintf(w, "Usage: %s\n\n%s\n", strings.TrimSpace(commandName(c.name)+" "+c.args), c.summary)
	fs := flag.NewFlagSet(commandName(c.name), flag.ContinueOnError)
	if c.flags != nil {
		c.flags(fs)
	}
	first := true
	fs.VisitAll(func(f *flag.Flag) {
		if first {
			fmt.Fprintln(w, "\nFlags:")
			first = false
		}
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		fmt.Fprintf(w, "  %s%s\n      %s\n", dash, f.Name, f.Usage)
	})
}

// stdin is what confirm reads, and whether it is a person at a terminal.
var stdin = struct {
	io.Reader
	terminal func() bool
}{os.Stdin, func() bool { return isTerminal(os.Stdin) }}

// confirm asks question before something that can't be undone. --yes answers
// it; with no terminal to ask at, it refuses rather than waiting or guessing.
func confirm(question string, yes bool) error {
	if yes {
		return nil
	}
	if !stdin.terminal() {
		return errors.New("this needs a yes: run it in a terminal, or add --yes")
	}
	fmt.Print(question + " [y/N] ")
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return errDeclined
	}
	return nil
}

// errDeclined is a "no" at confirm: nothing changed, and nothing failed.
var errDeclined = errors.New("left as it was")
