// Package cli is the command-line inbound adapter. It talks to the platform
// through the REST API (via internal/apiclient) and contains no business
// logic: it parses arguments, calls the API and renders results.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"personal-ai/internal/apiclient"
	"personal-ai/internal/apitypes"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// App runs CLI commands against an API client.
type App struct {
	Client *apiclient.Client
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

const usageText = `Usage: personal-ai [-json] <command> [arguments]

Commands:
  health                                   check that the server is up
  models list [-all]                       list the model catalog
  models get <id>                          show one model
  models register <file.json>              register a model ("-" reads stdin)
  models update <file.json>                replace a model definition
  models activate|deactivate <id>          change availability
  models validate <id>                     show the hardware it would run on
  default [<id>]                           show or set the default model
  deploy <model> [-pin] [-wait]            start a deployment
  deployments list [-status s]             list deployments
  deployments get|stop|restart|delete <id>
  deployments pin|unpin|health <id>
  usage [-since 24h | -from T -to T]       usage and estimated cost
  chat [-model id] [-system text] [-no-stream] [message...]
                                           chat; without a message, start an
                                           interactive session (/reset, /exit)

Environment: PERSONAL_AI_API_URL, PERSONAL_AI_MANAGEMENT_TOKEN,
PERSONAL_AI_INFERENCE_TOKEN, PERSONAL_AI_CONFIG.
`

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// Run executes one command and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("personal-ai", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	asJSON := fs.Bool("json", false, "print raw JSON")
	fs.Usage = func() { fmt.Fprint(a.Stderr, usageText) }
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(a.Stderr, usageText)
		return ExitUsage
	}
	err := a.dispatch(ctx, rest[0], rest[1:], *asJSON)
	var ue usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ue):
		fmt.Fprintf(a.Stderr, "error: %s\n\n%s", ue.msg, usageText)
		return ExitUsage
	default:
		fmt.Fprintln(a.Stderr, "error:", err)
		return ExitError
	}
}

func (a *App) dispatch(ctx context.Context, cmd string, args []string, asJSON bool) error {
	switch cmd {
	case "health":
		if err := a.Client.Health(ctx); err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, "ok")
		return nil
	case "models":
		return a.models(ctx, args, asJSON)
	case "default":
		return a.defaultModel(ctx, args)
	case "deploy":
		return a.deploy(ctx, args, asJSON)
	case "deployments":
		return a.deployments(ctx, args, asJSON)
	case "usage":
		return a.usage(ctx, args, asJSON)
	case "chat":
		return a.chat(ctx, args)
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usageText)
		return nil
	}
	return usagef("unknown command %q", cmd)
}

func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *App) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

func oneArg(args []string, what string) (string, error) {
	if len(args) != 1 || args[0] == "" {
		return "", usagef("expected exactly one %s", what)
	}
	return args[0], nil
}

// ---- models ----

func (a *App) models(ctx context.Context, args []string, asJSON bool) error {
	if len(args) == 0 {
		return usagef("models needs a subcommand")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("models list", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		all := fs.Bool("all", false, "include deactivated models")
		if err := fs.Parse(args); err != nil {
			return usagef("%v", err)
		}
		ms, err := a.Client.ListModels(ctx, *all)
		if err != nil {
			return err
		}
		if asJSON {
			return a.printJSON(ms)
		}
		rows := make([][]string, len(ms))
		for i, m := range ms {
			rows[i] = []string{m.ID, m.Name, m.Status, fmt.Sprintf("%d GB x%d", m.Requirements.MinVRAMGB, max(1, m.Requirements.MinGPUCount)), m.Artifact}
		}
		a.table("ID\tNAME\tSTATUS\tMIN VRAM\tARTIFACT", rows)
		return nil
	case "get":
		id, err := oneArg(args, "model id")
		if err != nil {
			return err
		}
		m, err := a.Client.GetModel(ctx, id)
		if err != nil {
			return err
		}
		return a.printJSON(m)
	case "register", "update":
		file, err := oneArg(args, "file")
		if err != nil {
			return err
		}
		m, err := a.readModel(file)
		if err != nil {
			return err
		}
		if sub == "register" {
			m, err = a.Client.RegisterModel(ctx, m)
		} else {
			m, err = a.Client.UpdateModel(ctx, m)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "model %s saved\n", m.ID)
		return nil
	case "activate", "deactivate":
		id, err := oneArg(args, "model id")
		if err != nil {
			return err
		}
		m, err := a.Client.SetModelActive(ctx, id, sub == "activate")
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "model %s is %s\n", m.ID, m.Status)
		return nil
	case "validate":
		id, err := oneArg(args, "model id")
		if err != nil {
			return err
		}
		c, err := a.Client.ValidateModel(ctx, id)
		if err != nil {
			return err
		}
		if asJSON {
			return a.printJSON(c)
		}
		cost := "unknown"
		if c.HourlyCostMicros != nil {
			cost = fmt.Sprintf("%s %.4f/h", c.Currency, float64(*c.HourlyCostMicros)/1e6)
		}
		fmt.Fprintf(a.Stdout, "model %s fits on %s: %d x %s (%d GB each), %s\n", id, c.ID, c.GPUCount, c.GPUModel, c.VRAMPerGPUGB, cost)
		return nil
	}
	return usagef("unknown models subcommand %q", sub)
}

func (a *App) readModel(file string) (apitypes.Model, error) {
	var r io.Reader = a.Stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return apitypes.Model{}, err
		}
		defer f.Close()
		r = f
	}
	var m apitypes.Model
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("parse %s: %w", file, err)
	}
	return m, nil
}

func (a *App) defaultModel(ctx context.Context, args []string) error {
	switch len(args) {
	case 0:
		id, err := a.Client.GetDefaultModel(ctx)
		if apiclient.IsCode(err, "no_default_model") {
			fmt.Fprintln(a.Stdout, "no default model set")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, id)
		return nil
	case 1:
		if err := a.Client.SetDefaultModel(ctx, args[0]); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "default model set to %s\n", args[0])
		return nil
	}
	return usagef("default takes at most one model id")
}

// ---- deployments ----

func (a *App) deploy(ctx context.Context, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	pin := fs.Bool("pin", false, "protect from idle shutdown")
	wait := fs.Bool("wait", false, "wait until the deployment is ready")
	// Allow "deploy <model> -wait" as well as "deploy -wait <model>".
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return usagef("%v", err)
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	model, err := oneArg(positional, "model id")
	if err != nil {
		return err
	}
	d, err := a.Client.CreateDeployment(ctx, apitypes.CreateDeployment{ModelID: model, Pinned: *pin})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "deployment %s created (%s)\n", d.ID, d.Status)
	if *wait {
		d, err = a.Client.WaitDeployment(ctx, d.ID, 5*time.Second, func(d apitypes.Deployment) {
			fmt.Fprintf(a.Stderr, "  %s %s\n", time.Now().Format("15:04:05"), d.Status)
		})
		if err != nil {
			return err
		}
	}
	if asJSON {
		return a.printJSON(d)
	}
	fmt.Fprintln(a.Stdout, d.ID)
	return nil
}

func (a *App) deployments(ctx context.Context, args []string, asJSON bool) error {
	if len(args) == 0 {
		return usagef("deployments needs a subcommand")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("deployments list", flag.ContinueOnError)
		fs.SetOutput(a.Stderr)
		status := fs.String("status", "", "filter by status")
		if err := fs.Parse(args); err != nil {
			return usagef("%v", err)
		}
		ds, err := a.Client.ListDeployments(ctx, *status)
		if err != nil {
			return err
		}
		if asJSON {
			return a.printJSON(ds)
		}
		rows := make([][]string, len(ds))
		for i, d := range ds {
			pinned := ""
			if d.Pinned {
				pinned = "yes"
			}
			idle := "-"
			if d.Status == "ready" || d.Status == "unhealthy" {
				idle = time.Since(d.LastActivityAt).Truncate(time.Second).String()
			}
			rows[i] = []string{d.ID, d.ModelID, d.Status, pinned, fmt.Sprint(d.ActiveRequests), idle, d.CreatedAt.Local().Format(time.DateTime)}
		}
		a.table("ID\tMODEL\tSTATUS\tPINNED\tACTIVE\tIDLE\tCREATED", rows)
		return nil
	case "get":
		id, err := oneArg(args, "deployment id")
		if err != nil {
			return err
		}
		d, err := a.Client.GetDeployment(ctx, id)
		if err != nil {
			return err
		}
		return a.printJSON(d)
	case "delete":
		id, err := oneArg(args, "deployment id")
		if err != nil {
			return err
		}
		if err := a.Client.DeleteDeployment(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "deployment %s deleted\n", id)
		return nil
	case "stop", "restart", "pin", "unpin", "health":
		id, err := oneArg(args, "deployment id")
		if err != nil {
			return err
		}
		d, err := a.Client.DeploymentAction(ctx, id, sub)
		if err != nil {
			return err
		}
		if asJSON {
			return a.printJSON(d)
		}
		fmt.Fprintf(a.Stdout, "deployment %s: %s (health %s, pinned %v)\n", d.ID, d.Status, d.Health, d.Pinned)
		return nil
	}
	return usagef("unknown deployments subcommand %q", sub)
}

// ---- usage ----

func (a *App) usage(ctx context.Context, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	since := fs.Duration("since", 0, "look back this long (e.g. 24h)")
	fromS := fs.String("from", "", "start time (RFC 3339)")
	toS := fs.String("to", "", "end time (RFC 3339)")
	if err := fs.Parse(args); err != nil {
		return usagef("%v", err)
	}
	var from, to time.Time
	var err error
	if *since > 0 {
		from = time.Now().Add(-*since)
	}
	if *fromS != "" {
		if from, err = time.Parse(time.RFC3339, *fromS); err != nil {
			return usagef("-from: %v", err)
		}
	}
	if *toS != "" {
		if to, err = time.Parse(time.RFC3339, *toS); err != nil {
			return usagef("-to: %v", err)
		}
	}
	s, err := a.Client.Usage(ctx, from, to)
	if err != nil {
		return err
	}
	if asJSON {
		return a.printJSON(s)
	}
	fmt.Fprintf(a.Stdout, "requests:        %d\n", s.Requests)
	fmt.Fprintf(a.Stdout, "tokens:          %d in / %d out / %d total\n", s.InputTokens, s.OutputTokens, s.TotalTokens)
	fmt.Fprintf(a.Stdout, "inference time:  %s\n", time.Duration(s.InferenceSeconds*float64(time.Second)).Truncate(time.Second))
	fmt.Fprintf(a.Stdout, "compute uptime:  %s\n", time.Duration(s.UptimeSeconds*float64(time.Second)).Truncate(time.Second))
	printCosts := func(label string, m map[string]int64) {
		if len(m) == 0 {
			fmt.Fprintf(a.Stdout, "%-16s none\n", label+":")
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(a.Stdout, "%-16s %s %.4f\n", label+":", k, float64(m[k])/1e6)
		}
	}
	printCosts("estimated cost", s.EstimatedCostMicros)
	if len(s.BilledCostMicros) > 0 {
		printCosts("billed cost", s.BilledCostMicros)
	}
	if s.UptimeRecordsWithUnknownCost > 0 {
		fmt.Fprintf(a.Stdout, "note: %d uptime period(s) had no known price and are not included in the cost\n", s.UptimeRecordsWithUnknownCost)
	}
	return nil
}

// ---- chat ----

func (a *App) chat(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	model := fs.String("model", "", "model id (default: the configured default model)")
	system := fs.String("system", "", "system prompt")
	noStream := fs.Bool("no-stream", false, "wait for the whole answer")
	if err := fs.Parse(args); err != nil {
		return usagef("%v", err)
	}
	var history []apitypes.ChatMessage
	if *system != "" {
		history = append(history, apitypes.ChatMessage{Role: "system", Content: apitypes.ChatContent(*system)})
	}
	if msg := strings.TrimSpace(strings.Join(fs.Args(), " ")); msg != "" {
		_, err := a.exchange(ctx, *model, append(history, apitypes.ChatMessage{Role: "user", Content: apitypes.ChatContent(msg)}), !*noStream)
		return err
	}

	// Interactive session.
	base := len(history)
	sc := bufio.NewScanner(a.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for {
		fmt.Fprint(a.Stderr, "> ")
		if !sc.Scan() {
			fmt.Fprintln(a.Stderr)
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/reset":
			history = history[:base]
			fmt.Fprintln(a.Stderr, "(conversation cleared)")
			continue
		}
		history = append(history, apitypes.ChatMessage{Role: "user", Content: apitypes.ChatContent(line)})
		reply, err := a.exchange(ctx, *model, history, !*noStream)
		if err != nil {
			history = history[:len(history)-1]
			fmt.Fprintln(a.Stderr, "error:", err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		history = append(history, apitypes.ChatMessage{Role: "assistant", Content: apitypes.ChatContent(reply)})
	}
}

// exchange sends the conversation and prints the answer, returning it.
func (a *App) exchange(ctx context.Context, model string, msgs []apitypes.ChatMessage, stream bool) (string, error) {
	req := apitypes.ChatRequest{Model: model, Messages: msgs}
	if !stream {
		resp, err := a.Client.Chat(ctx, req)
		if err != nil {
			return "", explain(err)
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("empty response")
		}
		text := string(resp.Choices[0].Message.Content)
		fmt.Fprintln(a.Stdout, text)
		return text, nil
	}
	var sb strings.Builder
	_, err := a.Client.ChatStream(ctx, req, func(delta string) {
		sb.WriteString(delta)
		fmt.Fprint(a.Stdout, delta)
	})
	fmt.Fprintln(a.Stdout)
	if err != nil {
		return sb.String(), explain(err)
	}
	return sb.String(), nil
}

// explain adds operator hints to common API errors.
func explain(err error) error {
	switch {
	case apiclient.IsCode(err, "model_not_deployed"):
		return fmt.Errorf("%w\nhint: start it with `personal-ai deploy <model> -wait`", err)
	case apiclient.IsCode(err, "model_starting"):
		return fmt.Errorf("%w\nhint: the model is starting; try again shortly", err)
	case apiclient.IsCode(err, "no_default_model"):
		return fmt.Errorf("%w\nhint: pass -model or run `personal-ai default <model>`", err)
	}
	return err
}
