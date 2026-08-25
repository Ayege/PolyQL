package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build information, set at link time by the Makefile:
//
//	-ldflags "-X main.version=... -X main.commit=... -X main.buildDate=..."
//
// The defaults are what a plain "go build" produces, and say so rather than
// claiming a version the binary does not have.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// buildInfo is the version in the shape the JSON format writes.
type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	// Languages are the DSLs this binary can read and write, which depends on
	// which packages it was built with.
	Languages []string `json:"languages"`
}

func newVersionCommand(opts *options) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version and what this binary can translate",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info := buildInfo{
				Version:   version,
				Commit:    commit,
				BuildDate: buildDate,
				GoVersion: runtime.Version(),
				Platform:  runtime.GOOS + "/" + runtime.GOARCH,
				Languages: translatableLanguages(),
			}

			if asJSON {
				encoder := json.NewEncoder(opts.stdout)
				encoder.SetIndent("", "  ")
				return encoder.Encode(info)
			}

			fmt.Fprintf(opts.stdout, "polyql %s\n", info.Version)
			fmt.Fprintf(opts.stdout, "  commit:    %s\n", info.Commit)
			fmt.Fprintf(opts.stdout, "  built:     %s\n", info.BuildDate)
			fmt.Fprintf(opts.stdout, "  go:        %s\n", info.GoVersion)
			fmt.Fprintf(opts.stdout, "  platform:  %s\n", info.Platform)
			fmt.Fprintf(opts.stdout, "  languages: %s\n", joinOr(info.Languages, "none"))
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}
