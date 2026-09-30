package main

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

// Actions a subcommand accepts right after its name. help is not listed with
// the real verbs: the help flags (`--help`, `-h`) and the help word reach it,
// while the bare subcommand shows the brief action index.
const (
	actionHelp   = "help"
	actionStart  = "start"
	actionStop   = "stop"
	actionStatus = "status"
	actionStdio  = "stdio"
)

// subcommand describes one leading command-line namespace: `termcp <name>
// <action> [flags]`, or `termcp <name> [value] [flags]` when it has no
// actions. Adding an entry to subcommands is all it takes to teach the parser
// a new namespace; main() decides what its actions do. Without a subcommand
// the arguments stay plain server flags.
type subcommand struct {
	name    string
	summary string   // one line for the top-level usage
	argHint string   // what follows the name, for the usage and error lines
	actions []string // the real verbs; help is implicit, empty for a value namespace
}

// subcommands lists the namespaces in the order they are shown to the user.
var subcommands = []subcommand{
	{name: "daemon", summary: "manage the background instance", argHint: "<action>", actions: []string{actionStart, actionStop, actionStatus, actionStdio}},
	{name: "stdio", summary: "bridge stdin/stdout MCP to an instance already answering", argHint: "[endpoint]"},
}

// findSubcommand looks a namespace up by its leading word.
func findSubcommand(name string) (subcommand, bool) {
	for _, sub := range subcommands {
		if sub.name == name {
			return sub, true
		}
	}
	return subcommand{}, false
}

// findAction reports which subcommand an action word belongs to, so a stray
// action can be pointed back at its namespace (`termcp start` →
// `termcp daemon start`).
func findAction(word string) (string, bool) {
	for _, sub := range subcommands {
		if slices.Contains(sub.actions, word) {
			return sub.name, true
		}
	}
	return "", false
}

// parseSubcommand picks the leading subcommand off args. It returns the
// namespace name (empty when there is none), the selected action (empty for
// the bare name — the brief action index for a namespaced one, the arguments
// themselves for a namespace without actions; the help word and the help
// flags select "help"), and the arguments left for the flag parser — this
// runs first because the flag package stops at the first positional
// argument.
func parseSubcommand(args []string) (name, action string, rest []string, err error) {
	if len(args) == 0 {
		return "", "", args, nil
	}
	sub, ok := findSubcommand(args[0])
	if !ok {
		return "", "", args, nil
	}
	if len(args) == 1 {
		return sub.name, "", nil, nil
	}
	if args[1] == actionHelp || slices.Contains(sub.actions, args[1]) {
		return sub.name, args[1], args[2:], nil
	}
	if isHelpFlag(args[1]) {
		return sub.name, actionHelp, args[2:], nil
	}
	if len(sub.actions) == 0 {
		// A namespace without verbs (`termcp stdio sse`): everything after the
		// name belongs to it — its values and flags alike.
		return sub.name, "", args[1:], nil
	}
	if strings.HasPrefix(args[1], "-") {
		return "", "", nil, fmt.Errorf("%s: the action comes right after the subcommand, before its flags: expected %s, e.g. termcp %s %s",
			sub.name, listWords(sub.actions), sub.name, sub.actions[0])
	}
	return "", "", nil, fmt.Errorf("%s: unknown action %q: expected %s (or help)", sub.name, args[1], listWords(sub.actions))
}

// isHelpFlag reports the flag spellings that ask for help — the forms the
// flag package itself would treat as help requests.
func isHelpFlag(arg string) bool {
	switch arg {
	case "-h", "--h", "-help", "--help":
		return true
	}
	return false
}

// listWords joins words the way prose does: "start, stop or status".
func listWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	default:
		return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
	}
}

// usage prints the top-level help: the plain server form and one line per
// subcommand — each carries the detail when run bare or with --help.
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintf(out, "  %-34s %s\n", "termcp [flags]", "run the server in the foreground")
	for _, sub := range subcommands {
		detail := "--help the parameters"
		if len(sub.actions) > 0 {
			detail = fmt.Sprintf("bare `termcp %s` lists the actions, --help the parameters", sub.name)
		}
		fmt.Fprintf(out, "  %-34s %s — %s\n", "termcp "+sub.name+" "+sub.argHint+" [flags]", sub.summary, detail)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	flag.PrintDefaults()
}
