package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"e2b-local/internal/orbctl"
)

const requestTimeout = 60 * time.Second

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	opts, rest, err := parseGlobalOptions(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usageError("command is required")
	}

	socketPath := strings.TrimSpace(opts.SocketPath)
	if socketPath == "" {
		socketPath = strings.TrimSpace(os.Getenv("MYORB_SCONRPC_SOCK"))
	}
	client := orbctl.NewClient(socketPath)

	cmd := rest[0]
	cmdArgs := rest[1:]
	switch cmd {
	case "list":
		return runList(ctx, client, cmdArgs, stdout)
	case "info":
		return runInfo(ctx, client, cmdArgs, stdout)
	case "clone":
		if len(cmdArgs) != 2 {
			return usageError("usage: myorb clone <source-vm> <dest-vm>")
		}
		return client.Clone(ctx, cmdArgs[0], cmdArgs[1])
	case "start":
		if len(cmdArgs) != 1 {
			return usageError("usage: myorb start <vm>")
		}
		return client.Start(ctx, cmdArgs[0])
	case "stop":
		if len(cmdArgs) != 1 {
			return usageError("usage: myorb stop <vm>")
		}
		return client.Stop(ctx, cmdArgs[0])
	case "delete":
		return runDelete(ctx, client, cmdArgs)
	case "config":
		return runConfig(ctx, client, cmdArgs)
	case "run":
		return errors.New("myorb run is not implemented through sconrpc.sock yet")
	default:
		return usageError(fmt.Sprintf("unknown command %q", cmd))
	}
}

type globalOptions struct {
	SocketPath string
}

func parseGlobalOptions(args []string) (globalOptions, []string, error) {
	var opts globalOptions
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--sock":
			i++
			if i >= len(args) {
				return opts, nil, usageError("--sock requires a value")
			}
			opts.SocketPath = args[i]
		case strings.HasPrefix(arg, "--sock="):
			opts.SocketPath = strings.TrimPrefix(arg, "--sock=")
		default:
			rest = append(rest, args[i:]...)
			return opts, rest, nil
		}
	}
	return opts, rest, nil
}

func runList(ctx context.Context, client *orbctl.Client, args []string, stdout io.Writer) error {
	format, rest, err := parseFormat(args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usageError("usage: myorb list [--format json]")
	}

	machines, err := client.ListMachines(ctx)
	if err != nil {
		return err
	}
	if format == "" {
		for _, machine := range machines {
			fmt.Fprintf(stdout, "%s\t%s\n", machine.Name, machine.State)
		}
		return nil
	}
	if format != "json" {
		return usageError(fmt.Sprintf("unsupported list format %q", format))
	}
	return writeJSON(stdout, machines)
}

func runInfo(ctx context.Context, client *orbctl.Client, args []string, stdout io.Writer) error {
	format, rest, err := parseFormat(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError("usage: myorb info [--format json] <vm>")
	}

	info, err := client.Info(ctx, rest[0])
	if err != nil {
		return err
	}
	if format == "" {
		fmt.Fprintf(stdout, "Name:\t%s\nState:\t%s\n", info.Record.Name, info.Record.State)
		return nil
	}
	if format != "json" {
		return usageError(fmt.Sprintf("unsupported info format %q", format))
	}
	return writeJSON(stdout, info)
}

func runDelete(ctx context.Context, client *orbctl.Client, args []string) error {
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			continue
		}
		rest = append(rest, arg)
	}
	if len(rest) != 1 {
		return usageError("usage: myorb delete [--force] <vm>")
	}
	return client.Delete(ctx, rest[0])
}

func runConfig(ctx context.Context, client *orbctl.Client, args []string) error {
	if len(args) == 0 {
		return usageError("usage: myorb config <set|add> ...")
	}

	switch args[0] {
	case "set":
		if len(args) != 3 {
			return usageError("usage: myorb config set machine.<vm>.isolated <true|false>")
		}
		machine, option, err := parseMachineSetting(args[1])
		if err != nil {
			return err
		}
		if option != "isolated" {
			return usageError(fmt.Sprintf("unsupported config option %q", option))
		}
		value, err := strconv.ParseBool(args[2])
		if err != nil {
			return usageError(fmt.Sprintf("invalid bool value %q", args[2]))
		}
		return client.SetIsolated(ctx, machine, value)
	case "add":
		if len(args) != 3 {
			return usageError("usage: myorb config add machine.<vm>.mounts <host-path>[:<vm-path>]")
		}
		machine, option, err := parseMachineSetting(args[1])
		if err != nil {
			return err
		}
		if option != "mounts" {
			return usageError(fmt.Sprintf("unsupported config option %q", option))
		}
		source, dest := parseMountSpec(args[2])
		return client.AddMount(ctx, machine, source, dest)
	default:
		return usageError(fmt.Sprintf("unsupported config command %q", args[0]))
	}
}

func parseFormat(args []string) (string, []string, error) {
	format := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--format" || arg == "-f":
			i++
			if i >= len(args) {
				return "", nil, usageError(arg + " requires a value")
			}
			format = args[i]
		case strings.HasPrefix(arg, "--format="):
			format = strings.TrimPrefix(arg, "--format=")
		default:
			rest = append(rest, arg)
		}
	}
	return strings.TrimSpace(format), rest, nil
}

func parseMachineSetting(value string) (string, string, error) {
	const prefix = "machine."
	if !strings.HasPrefix(value, prefix) {
		return "", "", usageError("config target must start with machine.")
	}

	rest := strings.TrimPrefix(value, prefix)
	machine, option, ok := strings.Cut(rest, ".")
	if !ok || strings.TrimSpace(machine) == "" || strings.TrimSpace(option) == "" {
		return "", "", usageError("config target must be machine.<vm>.<option>")
	}
	return machine, option, nil
}

func parseMountSpec(spec string) (string, string) {
	source, dest, ok := strings.Cut(spec, ":")
	if !ok {
		return strings.TrimSpace(spec), ""
	}
	return strings.TrimSpace(source), strings.TrimSpace(dest)
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

type usageError string

func (e usageError) Error() string {
	return string(e)
}
