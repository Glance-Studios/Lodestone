package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/Glance-Studios/Lodestone/internal/api"
)

// commands returns the command table. A function rather than a package-level var
// because the Run funcs reference each other's helpers, and a var would need an
// init() to break the cycle.
func commands() []Command {
	return []Command{
		{
			Name:    "status",
			Summary: "check the agent is up and report its version",
			Run:     runStatus,
		},
		{
			Name:    "deploy",
			Summary: "upload an artifact and roll it out, streaming progress",
			Usage:   "<file>",
			Run:     runDeploy,
		},
		{
			Name:    "push",
			Summary: "upload an artifact and record it, without deploying",
			Usage:   "<file>",
			Run:     runPush,
		},
		{
			Name:    "ledger",
			Summary: "list what has been published, newest first",
			Run:     runLedger,
		},
		{
			Name:    "login",
			Summary: "save a server address and token as a named context",
			Usage:   "[context]",
			Run:     runLogin,
		},
		{
			Name:    "contexts",
			Summary: "list the configured contexts",
			Run:     runContexts,
		},
		{
			Name:    "version",
			Summary: "print the client version",
			Run:     runVersion,
		},
	}
}

// -- status -------------------------------------------------------------------

func runStatus(ctx context.Context, env Env, g Globals, args []string) error {
	if err := noArgs("status", args); err != nil {
		return err
	}

	client, name, _, err := g.client()
	if err != nil {
		return err
	}

	ctx, cancel := WithShortTimeout(ctx)
	defer cancel()

	st, err := client.Status(ctx)
	if err != nil {
		return err
	}

	if g.JSON {
		return writeJSON(env.Out, st)
	}
	fmt.Fprintf(env.Out, "context  %s\n", name)
	fmt.Fprintf(env.Out, "status   %s\n", st.Status)
	fmt.Fprintf(env.Out, "version  %s\n", st.Version)
	fmt.Fprintf(env.Out, "uptime   %s\n", st.Uptime)
	if len(st.Targets) > 0 {
		fmt.Fprintf(env.Out, "targets  %s\n", strings.Join(st.Targets, ", "))
	} else {
		fmt.Fprintln(env.Out, "targets  none configured")
	}
	return nil
}

// -- deploy -------------------------------------------------------------------

func runDeploy(ctx context.Context, env Env, g Globals, args []string) error {
	path, err := oneFile("deploy", args)
	if err != nil {
		return err
	}

	client, _, cctx, err := g.client()
	if err != nil {
		return err
	}

	name, err := g.target(cctx)
	if err != nil {
		return usageErr(err.Error())
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	opts := UploadOptions{
		Version:  os.Getenv("LODESTONE_VERSION"),
		By:       whoami(),
		Replicas: g.replicas(),
	}

	// In JSON mode stay silent until the end, so stdout is a single parseable
	// document. Otherwise narrate as the events arrive.
	onEvent := func(e api.Event) {}
	if !g.JSON {
		fmt.Fprintf(env.Out, "deploying %s to %s", path, name)
		if opts.Replicas != nil {
			fmt.Fprintf(env.Out, " (%d replicas)", *opts.Replicas)
		}
		fmt.Fprintln(env.Out)

		onEvent = func(e api.Event) {
			fmt.Fprintf(env.Out, "  %-13s %s\n", e.Phase, e.Message)
			if e.Error != "" {
				fmt.Fprintf(env.Out, "  %-13s %s\n", "", e.Error)
			}
		}
	}

	res, err := client.Deploy(ctx, name, f, opts, onEvent)
	if err != nil {
		return err
	}

	if g.JSON {
		if err := writeJSON(env.Out, res); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(env.Out)
		fmt.Fprintf(env.Out, "artifact  %s\n", res.Digest)
		if res.Image != "" {
			fmt.Fprintf(env.Out, "image     %s\n", res.Image)
		}
		// Always show the base by digest. With a moving base tag this is the only
		// record of which world the build went onto.
		if res.BaseImage != "" {
			fmt.Fprintf(env.Out, "base      %s\n", res.BaseImage)
		}
		if res.Deployed {
			fmt.Fprintln(env.Out, "result    deployed")
		} else {
			fmt.Fprintln(env.Out, "result    NOT deployed")
		}
	}

	if !res.Deployed {
		// A distinct exit code: the agent worked, the artifact is not live.
		return &ExitCode{
			Code: ExitNotHealthy,
			Err:  fmt.Errorf("deploy did not stick: %s", res.Error),
		}
	}
	return nil
}

// -- push ---------------------------------------------------------------------

func runPush(ctx context.Context, env Env, g Globals, args []string) error {
	path, err := oneFile("push", args)
	if err != nil {
		return err
	}

	client, _, cctx, err := g.client()
	if err != nil {
		return err
	}

	name, err := g.target(cctx)
	if err != nil {
		return usageErr(err.Error())
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	art, err := client.Push(ctx, name, f, UploadOptions{
		Version: os.Getenv("LODESTONE_VERSION"),
		By:      whoami(),
	})
	if err != nil {
		return err
	}

	if g.JSON {
		return writeJSON(env.Out, art)
	}
	fmt.Fprintf(env.Out, "%s  (%s)\n", art.Digest, humanSize(art.Size))
	return nil
}

// -- ledger -------------------------------------------------------------------

func runLedger(ctx context.Context, env Env, g Globals, args []string) error {
	if err := noArgs("ledger", args); err != nil {
		return err
	}

	client, _, cctx, err := g.client()
	if err != nil {
		return err
	}

	name, err := g.target(cctx)
	if err != nil {
		return usageErr(err.Error())
	}

	ctx, cancel := WithShortTimeout(ctx)
	defer cancel()

	entries, err := client.Ledger(ctx, name)
	if err != nil {
		return err
	}

	if g.JSON {
		return writeJSON(env.Out, entries)
	}
	if len(entries) == 0 {
		fmt.Fprintln(env.Out, "nothing published yet")
		return nil
	}

	fmt.Fprintf(env.Out, "%-19s  %-12s  %-10s  %-8s  %s\n", "WHEN", "VERSION", "BY", "SIZE", "DIGEST")
	for _, e := range entries {
		version := e.Version
		if version == "" {
			version = "-"
		}
		by := e.By
		if by == "" {
			by = "-"
		}
		fmt.Fprintf(env.Out, "%-19s  %-12s  %-10s  %-8s  %s\n",
			e.At.Local().Format("2006-01-02 15:04:05"),
			version, by, humanSize(e.Size), shortDigest(e.Digest))
	}
	return nil
}

// -- login --------------------------------------------------------------------

func runLogin(ctx context.Context, env Env, g Globals, args []string) error {
	if len(args) > 1 {
		return usageErr("login takes at most one context name")
	}

	name := "default"
	if len(args) == 1 {
		name = args[0]
	}

	if g.API == "" {
		return usageErr("login needs --api (and usually --token)")
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.Set(name, Context{API: g.API, Token: g.Token, Target: g.Target})
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "saved context %q -> %s", name, g.API)
	if g.Target != "" {
		fmt.Fprintf(env.Out, " (target %s)", g.Target)
	}
	fmt.Fprintln(env.Out)
	fmt.Fprintf(env.Out, "config %s\n", cfg.Path())

	if g.Token == "" {
		fmt.Fprintln(env.Err, "warning: no --token given; protected commands will be rejected")
	}
	if g.Target == "" {
		fmt.Fprintln(env.Err, "warning: no --target given; commands will need --target")
	}
	return nil
}

// -- contexts -----------------------------------------------------------------

func runContexts(ctx context.Context, env Env, g Globals, args []string) error {
	if err := noArgs("contexts", args); err != nil {
		return err
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	if g.JSON {
		return writeJSON(env.Out, cfg)
	}

	if api := os.Getenv("LODESTONE_API"); api != "" {
		fmt.Fprintf(env.Out, "LODESTONE_API is set (%s); it overrides every context below\n\n", api)
	}
	if len(cfg.Contexts) == 0 {
		fmt.Fprintln(env.Out, "no contexts configured - run `lodestone login --api <url> --token <tok>`")
		return nil
	}

	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	slices.Sort(names)

	for _, n := range names {
		marker := "  "
		if n == cfg.Default {
			marker = "* "
		}
		c := cfg.Contexts[n]

		// Never print the token. Report only whether one is set.
		token := "no token"
		if c.Token != "" {
			token = "token set"
		}
		tgt := c.Target
		if tgt == "" {
			tgt = "-"
		}
		fmt.Fprintf(env.Out, "%s%-12s %-30s %-14s %s\n", marker, n, c.API, tgt, token)
	}
	return nil
}

// -- version ------------------------------------------------------------------

func runVersion(ctx context.Context, env Env, g Globals, args []string) error {
	if err := noArgs("version", args); err != nil {
		return err
	}
	if g.JSON {
		return writeJSON(env.Out, map[string]string{"version": env.Version})
	}
	fmt.Fprintf(env.Out, "lodestone %s\n", env.Version)
	return nil
}

// -- helpers ------------------------------------------------------------------

func usageErr(msg string) error {
	return &ExitCode{Code: ExitUsage, Err: fmt.Errorf("%s", msg)}
}

func noArgs(name string, args []string) error {
	if len(args) > 0 {
		return usageErr(fmt.Sprintf("%s takes no arguments, got %q", name, strings.Join(args, " ")))
	}
	return nil
}

func oneFile(name string, args []string) (string, error) {
	switch len(args) {
	case 1:
		return args[0], nil
	case 0:
		return "", usageErr(fmt.Sprintf("%s needs a file", name))
	default:
		return "", usageErr(fmt.Sprintf("%s takes one file, got %d", name, len(args)))
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

// whoami labels a ledger entry. LODESTONE_BY wins so CI can identify itself.
func whoami() string {
	if by := os.Getenv("LODESTONE_BY"); by != "" {
		return by
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("USERNAME") // windows
}

func shortDigest(d string) string {
	hex := strings.TrimPrefix(d, "sha256:")
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB"}
	value := float64(n)
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}
