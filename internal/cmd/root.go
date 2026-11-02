// Package cmd wires the cobra commands of the plugin.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/klahio/kubectl-inferon/internal/version"
)

// PluginName is the plugin's name; see version.Name.
const PluginName = version.Name

// Exit codes, as defined in docs/mvp.md section 3.
const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitClusterOrNF = 3
)

// ExitError carries an exit code up to Execute. Commands return it instead of calling os.Exit.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Options shared by every subcommand.
type rootOptions struct {
	configFlags *genericclioptions.ConfigFlags
	streams     genericiooptions.IOStreams
	noColor     bool
}

// Execute runs the plugin and returns the process exit code.
func Execute() int {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	root := newRootCommand(streams)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		// A failure result (for example "not Ready" with --exit-code) has already been printed; there is no message to add.
		if exitErr.Err != nil {
			fmt.Fprintln(streams.ErrOut, "error:", exitErr.Err)
		}
		return exitErr.Code
	}
	// Anything cobra itself rejects (unknown flag, wrong number of arguments) is a usage error.
	fmt.Fprintln(streams.ErrOut, "error:", err)
	return ExitUsage
}

func newRootCommand(streams genericiooptions.IOStreams) *cobra.Command {
	opts := &rootOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
		streams:     streams,
	}

	root := &cobra.Command{
		Use: PluginName,
		// cobra takes the command name from the first word of Use; the display name makes help read "kubectl inferon".
		Annotations: map[string]string{cobra.CommandDisplayNameAnnotation: commandPrefix() + PluginName},
		Short:       "Diagnose machine learning model inference services",
		Version:     version.String(),
		Long: "Diagnose machine learning model inference services on Kubernetes.\n" +
			"list prints them as TYPE/NAME; status, test and logs take that name.\n" +
			"Supported types: isvc (InferenceService) and llmisvc (LLMInferenceService).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.ErrOut)

	opts.configFlags.AddFlags(root.PersistentFlags())
	root.PersistentFlags().BoolVar(&opts.noColor, "no-color", false, "Turn off colored output (the NO_COLOR environment variable does the same)")

	root.SetVersionTemplate("{{.DisplayName}} {{.Version}}\n")
	root.AddCommand(newListCommand(opts), newStatusCommand(opts), newTestCommand(opts), newLogsCommand(opts), &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), commandPrefix()+PluginName, version.String())
		},
	})
	return root
}

// commandPrefix makes help text read "kubectl inferon ..." when the binary runs as a kubectl plugin.
func commandPrefix() string {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		return "kubectl "
	}
	return ""
}

// useColor decides whether output written to w may contain color escape codes.
func (o *rootOptions) useColor(w io.Writer) bool {
	if o.noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
