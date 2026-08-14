package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// Env is everything a command needs from the outside world. Passing it in rather
// than reaching for os.Stdout directly is what makes the commands testable.
type Env struct {
	Out     io.Writer
	Err     io.Writer
	Version string
}

// Command is one subcommand.
type Command struct {
	Name    string
	Summary string // one line, shown in the command list
	Usage   string // argument shape, e.g. "<file>"

	// Run receives the already-parsed flags for this command.
	Run func(ctx context.Context, env Env, g Globals, args []string) error
}

// Globals are the flags accepted by every command.
type Globals struct {
	Ctx    string // --ctx: named context to use
	API    string // --api: override the address
	Token  string // --token: override the token
	Target string // --target: which workload to address
	JSON   bool   // --json: machine-readable output

	// Replicas is -1 when unset, because 0 is a meaningful count (scale to
	// nothing) and a bare int cannot distinguish the two.
	Replicas int
}

// replicas returns the requested count, or nil when none was asked for.
func (g Globals) replicas() *int32 {
	if g.Replicas < 0 {
		return nil
	}
	n := int32(g.Replicas)
	return &n
}

// target resolves which workload to address: the flag wins, then the context's
// default target.
func (g Globals) target(ctx Context) (string, error) {
	if g.Target != "" {
		return g.Target, nil
	}
	if ctx.Target != "" {
		return ctx.Target, nil
	}
	return "", fmt.Errorf("no target given: pass --target, or set one with `lodestone login`")
}

// client resolves a context and returns a client for it, plus the context's
// name and the resolved context itself. Explicit --api/--token win over anything
// stored, so a one-off call needs no config change.
func (g Globals) client() (*Client, string, Context, error) {
	if g.API != "" {
		c := Context{API: g.API, Token: g.Token, Target: g.Target}
		return NewClient(c), "flags", c, nil
	}

	cfg, err := LoadConfig()
	if err != nil {
		return nil, "", Context{}, err
	}

	ctx, name, err := cfg.Resolve(g.Ctx)
	if err != nil {
		return nil, "", Context{}, err
	}
	if g.Token != "" {
		ctx.Token = g.Token
	}
	return NewClient(ctx), name, ctx, nil
}

// ExitCode is an error carrying a specific process exit status. Commands return
// it when the failure is an outcome rather than a fault - a rolled-back deploy
// is a working agent reporting bad news, and needs its own code so scripts can
// tell it from a crash.
type ExitCode struct {
	Code int
	Err  error
}

func (e *ExitCode) Error() string { return e.Err.Error() }
func (e *ExitCode) Unwrap() error { return e.Err }

// Exit codes. Kept small and documented, because they are part of the interface.
const (
	ExitOK         = 0
	ExitError      = 1  // anything unexpected
	ExitNotHealthy = 2  // the deploy ran and was rolled back
	ExitUsage      = 64 // bad invocation, following sysexits(3)
)

// Run parses args (without the program name) and runs the requested command.
// It returns the process exit code.
func Run(ctx context.Context, env Env, args []string) int {
	// Replicas starts at -1 to mean "not asked for", because 0 is a meaningful
	// count and a bare int cannot distinguish the two.
	g := Globals{Replicas: -1}

	// The global flag set also owns -h/--help for the top level.
	fs := flag.NewFlagSet("lodestone", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	registerGlobals(fs, &g)
	fs.Usage = func() { printRootUsage(env, fs) }

	if err := fs.Parse(args); err != nil {
		// ContinueOnError already printed the problem; -h is not a failure.
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}

	rest := fs.Args()
	if len(rest) == 0 {
		printRootUsage(env, fs)
		return ExitUsage
	}

	name, cmdArgs := rest[0], rest[1:]

	cmd, ok := lookup(name)
	if !ok {
		fmt.Fprintf(env.Err, "lodestone: unknown command %q\n\n", name)
		printRootUsage(env, fs)
		return ExitUsage
	}

	// Each command gets its own flag set, so global flags may appear on either
	// side of the command name - `lodestone --ctx dev status` and
	// `lodestone status --ctx dev` both work, which is what people expect.
	sub := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	sub.SetOutput(env.Err)
	registerGlobals(sub, &g)
	sub.Usage = func() { printCommandUsage(env, cmd, sub) }

	positional, err := parseInterspersed(sub, cmdArgs)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}

	if err := cmd.Run(ctx, env, g, positional); err != nil {
		return report(env, err)
	}
	return ExitOK
}

// parseInterspersed parses flags that may appear before, after, or among the
// positional arguments, and returns the positionals.
//
// Stdlib flag stops at the first non-flag argument, so a plain Parse would treat
// everything in `deploy plugin.jar --api http://x` after the filename as
// positional - including the flag. Parsing, taking one positional, and parsing
// again handles both orders, which is what people actually type.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string

	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}

		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}

		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// report prints an error the way a CLI should and picks the exit code.
func report(env Env, err error) int {
	var exit *ExitCode
	if errors.As(err, &exit) {
		fmt.Fprintf(env.Err, "lodestone: %v\n", exit.Err)
		return exit.Code
	}

	fmt.Fprintf(env.Err, "lodestone: %v\n", err)

	// Turn the two failures a user can actually fix into advice, rather than
	// leaving them to interpret a status code.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Unauthorized():
			fmt.Fprintln(env.Err, "  the token was rejected - check `lodestone contexts` or run `lodestone login`")
		case apiErr.Busy():
			fmt.Fprintln(env.Err, "  another deploy is in progress - wait for it to finish and retry")
		}
	}
	if errors.Is(err, ErrNoContext) {
		fmt.Fprintln(env.Err, "  run `lodestone login` or set LODESTONE_API and LODESTONE_TOKEN")
	}

	return ExitError
}

func registerGlobals(fs *flag.FlagSet, g *Globals) {
	fs.StringVar(&g.Ctx, "ctx", g.Ctx, "named context to use")
	fs.StringVar(&g.API, "api", g.API, "server address, overriding the context")
	fs.StringVar(&g.Token, "token", g.Token, "bearer token, overriding the context")
	fs.StringVar(&g.Target, "target", g.Target, "target to address, overriding the context")
	// Default carried from g, not a literal: this runs twice - once for the root
	// flag set and once for the subcommand - and a literal would discard what the
	// root already parsed.
	fs.IntVar(&g.Replicas, "replicas", g.Replicas, "scale the target to this many instances")
	fs.BoolVar(&g.JSON, "json", g.JSON, "machine-readable output")
}

func lookup(name string) (Command, bool) {
	for _, c := range commands() {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

func printRootUsage(env Env, fs *flag.FlagSet) {
	w := env.Err
	fmt.Fprintf(w, "lodestone %s - client for the Lodestone deploy agent\n\n", env.Version)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  lodestone [flags] <command> [arguments]")
	fmt.Fprintln(w, "\nCommands:")

	cmds := commands()
	slices.SortFunc(cmds, func(a, b Command) int { return strings.Compare(a.Name, b.Name) })

	width := 0
	for _, c := range cmds {
		if n := len(c.Name); n > width {
			width = n
		}
	}
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Summary)
	}

	fmt.Fprintln(w, "\nFlags:")
	fs.PrintDefaults()

	fmt.Fprintln(w, "\nEnvironment:")
	fmt.Fprintln(w, "  LODESTONE_API      server address; overrides the config file entirely")
	fmt.Fprintln(w, "  LODESTONE_TOKEN    bearer token")
	fmt.Fprintln(w, "  LODESTONE_TARGET   target name; used with LODESTONE_API")
	fmt.Fprintln(w, "  LODESTONE_VERSION  version string stamped onto the ledger entry")
	fmt.Fprintln(w, "  LODESTONE_CONFIG   config file path")
	fmt.Fprintln(w, "\nRun `lodestone <command> -h` for a command's own help.")
}

func printCommandUsage(env Env, cmd Command, fs *flag.FlagSet) {
	w := env.Err
	fmt.Fprintf(w, "%s - %s\n\n", cmd.Name, cmd.Summary)
	fmt.Fprintf(w, "Usage:\n  lodestone %s", cmd.Name)
	if cmd.Usage != "" {
		fmt.Fprintf(w, " %s", cmd.Usage)
	}
	fmt.Fprintln(w, "\n\nFlags:")
	fs.PrintDefaults()
}

// Main is the entry point cmd/lodestone calls.
func Main(version string) int {
	env := Env{Out: os.Stdout, Err: os.Stderr, Version: version}
	return Run(context.Background(), env, os.Args[1:])
}
