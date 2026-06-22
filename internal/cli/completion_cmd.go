package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/charmbracelet/huh"
	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/spf13/cobra"
)

var completionShells = []string{"bash", "zsh", "fish", "powershell"}

// completionMarker tags managed lines this command appends to a shell rc/profile.
const completionMarker = "# added by ai completion (AI Development Platform)"

// newCompletionCmd builds `ai completion <shell>` (CLI §1.7). Unlike cobra's
// default, it INSTALLS the script into the shell's completion location (and wires
// it up where needed) rather than only printing it. `--print` keeps the
// print-to-stdout behavior for piping.
func newCompletionCmd(emitter *output.Emitter, exit *int) *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Install shell completion for ai (or --print the script)",
		Long: "Install shell completion for ai (or --print the script). Run with no shell\n" +
			"argument on a terminal to be prompted to pick one; pass the shell as an\n" +
			"argument for non-interactive/scripted use. --print is unchanged.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: completionShells,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Prompt for the shell when it was not given and we are interactive;
			// otherwise the positional arg is required (§21).
			shell := ""
			if len(args) == 1 {
				shell = args[0]
			}
			if shell == "" {
				if !interactive(emitter) {
					*exit = emitter.Failure("completion", output.Errorf(output.ExitInvalidInput,
						"specify a shell (one of %v)", completionShells))
					return nil
				}
				options := make([]huh.Option[string], 0, len(completionShells))
				for _, candidate := range completionShells {
					options = append(options, huh.NewOption(candidate, candidate))
				}
				picked, err := promptChoice("Shell", "install ai completion for which shell", options, completionShells[0])
				if err != nil {
					*exit = emitter.Failure("completion", err)
					return nil
				}
				shell = picked
			}
			if !slices.Contains(completionShells, shell) {
				*exit = emitter.Failure("completion",
					output.Errorf(output.ExitInvalidInput, "unsupported shell %q (one of %v)", shell, completionShells))
				return nil
			}
			script, err := generateCompletion(cmd.Root(), shell)
			if err != nil {
				*exit = emitter.Failure("completion", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			if printOnly {
				_, _ = emitter.Out.Write(script)
				*exit = output.ExitOK
				return nil
			}
			path, hint, err := installCompletion(shell, script)
			if err != nil {
				*exit = emitter.Failure("completion", output.Errorf(output.ExitRuntimeFailure, "%s", err))
				return nil
			}
			*exit = emitter.Success("completion", completionResult{Shell: shell, InstalledPath: path, Note: hint})
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the completion script to stdout instead of installing it")
	return cmd
}

type completionResult struct {
	Shell         string `json:"shell"`
	InstalledPath string `json:"installed_path"`
	Note          string `json:"note"`
}

func (result completionResult) Human() string {
	return fmt.Sprintf("Installed %s completion → %s\n%s", result.Shell, result.InstalledPath, result.Note)
}

func generateCompletion(root *cobra.Command, shell string) ([]byte, error) {
	var buffer bytes.Buffer
	var err error
	switch shell {
	case "bash":
		err = root.GenBashCompletionV2(&buffer, true)
	case "zsh":
		err = root.GenZshCompletion(&buffer)
	case "fish":
		err = root.GenFishCompletion(&buffer, true)
	case "powershell":
		err = root.GenPowerShellCompletionWithDesc(&buffer)
	}
	return buffer.Bytes(), err
}

// installCompletion writes the script to the shell's standard completion location
// (creating dirs and, for zsh/powershell, wiring it into the rc/profile) and
// returns the installed path plus a one-line activation hint.
func installCompletion(shell string, script []byte) (installedPath, hint string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	switch shell {
	case "fish":
		path := filepath.Join(configHome(home), "fish", "completions", "ai.fish")
		if err := writeCompletionFile(path, script); err != nil {
			return "", "", err
		}
		return path, "Restart fish (or open a new shell).", nil

	case "bash":
		path := filepath.Join(dataHome(home), "bash-completion", "completions", "ai")
		if err := writeCompletionFile(path, script); err != nil {
			return "", "", err
		}
		return path, "Restart bash (requires the bash-completion package).", nil

	case "zsh":
		dir := filepath.Join(home, ".zsh", "completions")
		path := filepath.Join(dir, "_ai")
		if err := writeCompletionFile(path, script); err != nil {
			return "", "", err
		}
		block := fmt.Sprintf("%s\nfpath=(%q $fpath)\nautoload -Uz compinit && compinit\n", completionMarker, dir)
		if err := appendManaged(filepath.Join(home, ".zshrc"), block); err != nil {
			return "", "", err
		}
		return path, "Restart zsh (or run: exec zsh).", nil

	case "powershell":
		scriptPath := filepath.Join(configHome(home), "powershell", "ai.completion.ps1")
		if err := writeCompletionFile(scriptPath, script); err != nil {
			return "", "", err
		}
		profile := filepath.Join(configHome(home), "powershell", "Microsoft.PowerShell_profile.ps1")
		block := fmt.Sprintf("%s\n. %q\n", completionMarker, scriptPath)
		if err := appendManaged(profile, block); err != nil {
			return "", "", err
		}
		return scriptPath, "Restart PowerShell.", nil
	}
	return "", "", fmt.Errorf("unsupported shell %q", shell)
}

func configHome(home string) string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".config")
}

func dataHome(home string) string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".local", "share")
}

func writeCompletionFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// appendManaged appends block to a shell rc/profile if our marker is not already
// present (idempotent). Creates the file/dir if needed.
func appendManaged(rcPath, block string) error {
	if err := os.MkdirAll(filepath.Dir(rcPath), 0o755); err != nil {
		return err
	}
	if existing, err := os.ReadFile(rcPath); err == nil {
		if bytes.Contains(existing, []byte(completionMarker)) {
			return nil // already wired
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(rcPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.WriteString("\n" + block)
	return err
}
