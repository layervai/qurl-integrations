package main

import (
	frplog "github.com/fatedier/frp/pkg/util/log"
	goliblog "github.com/fatedier/golib/log"
)

// redirectFRPLogsToStderr keeps the embedded tunnel engine from writing
// diagnostics to stdout, which is reserved for command data.
func redirectFRPLogsToStderr(opts *globalOpts) {
	level := goliblog.WarnLevel
	if opts.verbose {
		level = goliblog.InfoLevel
	}
	frplog.Logger = goliblog.New(
		goliblog.WithCaller(false),
		goliblog.WithLevel(level),
		goliblog.WithOutput(goliblog.NewConsoleWriter(goliblog.ConsoleConfig{}, opts.streams.Err)),
	)
}
