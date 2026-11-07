/*
Copyright 2026 lburgazzoli.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package operator contains the standalone service runtime:
// the `operator` subcommand that starts one controller-runtime manager
// wrapping both discovery controllers through the framework builder.
package operator

import (
	"github.com/spf13/cobra"
)

// NewRootCommand builds the manager binary's root command. The only
// subcommand is `operator` — the canonical container command is
// [/manager, operator] — so an argument-less invocation prints help
// instead of silently starting a manager.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "manager",
		Short: "OpenDataHub hardware discovery service",
		Long: "OpenDataHub hardware discovery service: discovers accelerator " +
			"devices through the stable DRA and classic label planes and " +
			"reconciles them as HardwareDevice resources.",
	}
	root.AddCommand(NewCommand())
	return root
}

// NewCommand builds the `operator` subcommand with default options: the
// standalone manager running both discovery controllers — and only
// those — with leader election, health probes and no webhook server.
func NewCommand() *cobra.Command {
	options := newOptions()
	return newCommand(&options)
}

// newCommand builds the `operator` subcommand over the given options;
// the command's flags bind to that options value, so a caller that
// constructs its own options observes the parsed flags on it.
func newCommand(options *Options) *cobra.Command {
	command := &cobra.Command{
		Use:   "operator",
		Short: "Run the hardware discovery manager (both controllers)",
		RunE: func(command *cobra.Command, _ []string) error {
			return run(*options)
		},
	}
	options.bind(command.Flags())
	return command
}
