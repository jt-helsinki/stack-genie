package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/spf13/cobra"
)

func TestEmitJSONHelp(test *testing.T) {
	out := &bytes.Buffer{}
	emitter := &output.Emitter{JSON: true, Out: out, Err: &bytes.Buffer{}}

	root := &cobra.Command{Use: "ai", Short: "root", Long: "the ai cli"}
	root.Flags().Bool("json", false, "machine-readable output")
	root.AddCommand(&cobra.Command{Use: "project", Short: "Create, list, and delete projects"})

	code := emitJSONHelp(emitter, root)
	if code != output.ExitOK {
		test.Fatalf("exit = %d, want %d", code, output.ExitOK)
	}

	var envelope struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Data    struct {
			Help struct {
				Name  string `json:"name"`
				Path  string `json:"path"`
				Flags []struct {
					Name string `json:"name"`
				} `json:"flags"`
				Subcommands []struct {
					Name string `json:"name"`
				} `json:"subcommands"`
			} `json:"help"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		test.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if !envelope.OK || envelope.Command != "help" {
		test.Fatalf("envelope: %+v", envelope)
	}
	if envelope.Data.Help.Name != "ai" || envelope.Data.Help.Path != "ai" {
		test.Fatalf("help: %+v", envelope.Data.Help)
	}
	hasJSONFlag := false
	for _, flag := range envelope.Data.Help.Flags {
		if flag.Name == "json" {
			hasJSONFlag = true
		}
	}
	if !hasJSONFlag {
		test.Error("expected the json flag in data.help.flags")
	}
	if len(envelope.Data.Help.Subcommands) != 1 || envelope.Data.Help.Subcommands[0].Name != "project" {
		test.Fatalf("subcommands: %+v", envelope.Data.Help.Subcommands)
	}
}
