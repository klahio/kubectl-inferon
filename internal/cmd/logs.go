package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/klahio/kubectl-inferon/internal/logs"
)

type logsOptions struct {
	*rootOptions
	kind Kind
	logs.Options
	prefix string
}

func newLogsCommand(root *rootOptions) *cobra.Command {
	o := &logsOptions{rootOptions: root}
	c := &cobra.Command{
		Use:   "logs (isvc|llmisvc)/NAME",
		Short: "Show the logs of an inference service's containers without looking up pod or container names",
		Example: fmt.Sprintf(`  %[1]s%[2]s logs isvc/sklearn-iris            # model server of every predictor pod
  %[1]s%[2]s logs isvc/sklearn-iris --init     # model download (storage-initializer)
  %[1]s%[2]s logs isvc/sklearn-iris -c all -f  # follow every component
  %[1]s%[2]s logs isvc/sklearn-iris -p         # the previous, crashed instance
  %[1]s%[2]s logs llmisvc/smollm               # vLLM server (container main) of an LLMInferenceService`, commandPrefix(), PluginName),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := resourceArg(args[0])
			if err != nil {
				return err
			}
			o.kind = kind
			return o.run(cmd.Context(), name)
		},
	}
	f := c.Flags()
	f.StringVarP(&o.Component, "component", "c", "", "Component. InferenceService: predictor (default), transformer, explainer, all. LLMInferenceService: main (default), prefill, scheduler, worker, all")
	f.BoolVar(&o.Init, "init", false, "Show the storage-initializer (model download) logs instead of the server's")
	f.BoolVar(&o.AllContainers, "all-containers", false, "Show every container in the selected pods, init containers and sidecars included")
	f.BoolVarP(&o.Follow, "follow", "f", false, "Keep streaming; new pods and restarted containers are picked up")
	f.DurationVar(&o.Since, "since", 0, "Only logs newer than this duration, for example 10m")
	f.Int64Var(&o.Tail, "tail", 100, "Lines from the end of each container's log; -1 for all")
	f.BoolVarP(&o.Previous, "previous", "p", false, "Logs of the previous, terminated container instance")
	f.StringVar(&o.prefix, "prefix", "auto", "Prefix lines with [component/pod/container]: auto (when more than one stream), true or false")
	return c
}

func (o *logsOptions) run(ctx context.Context, name string) error {
	o.Layout = logs.InferenceServiceLayout
	if o.kind == KindLLMInferenceService {
		o.Layout = logs.LLMInferenceServiceLayout
	}
	if o.Component == "" {
		o.Component = o.Layout.Components[0]
	}
	if !slices.Contains(o.Layout.Accepted(), o.Component) {
		return usage("unknown component %q for an %s (use %s)", o.Component, o.Layout.Kind, strings.Join(o.Layout.Accepted(), ", "))
	}
	if o.Init && o.AllContainers {
		return &ExitError{Code: ExitUsage, Err: errors.New("--init and --all-containers cannot be used together")}
	}
	if o.Follow && o.Previous {
		return &ExitError{Code: ExitUsage, Err: errors.New("--follow and --previous cannot be used together")}
	}
	switch o.prefix {
	case "auto":
	case "true", "false":
		v := o.prefix == "true"
		o.Prefix = &v
	default:
		return &ExitError{Code: ExitUsage, Err: fmt.Errorf("--prefix must be auto, true or false, not %q", o.prefix)}
	}
	o.Color = o.useColor(o.streams.Out)
	o.PollInterval = 2 * time.Second

	if ctx == nil {
		ctx = context.Background()
	}
	clients, namespace, err := o.clients()
	if err != nil {
		return err
	}
	if o.kind == KindLLMInferenceService {
		_, err = clients.GetLLMInferenceService(ctx, namespace, name)
	} else {
		_, err = clients.GetInferenceService(ctx, namespace, name)
	}
	if err != nil {
		return clusterError(err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s := &logs.Streamer{
		Kube:      clients.Kube,
		Namespace: namespace,
		Name:      name,
		Options:   o.Options,
		Out:       o.streams.Out,
		Notes:     o.streams.ErrOut,
	}
	if err := s.Run(ctx); err != nil {
		if errors.Is(err, logs.ErrNoPods) {
			return &ExitError{Code: ExitFailure, Err: err}
		}
		if errors.Is(err, logs.ErrNoneReadable) {
			return &ExitError{Code: ExitFailure}
		}
		return clusterError(err)
	}
	return nil
}
