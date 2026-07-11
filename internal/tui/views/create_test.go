package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jt-helsinki/stack-genie/internal/ollama"
	"github.com/jt-helsinki/stack-genie/internal/state"
)

// seedProject writes a project root at <parent>/<name> (mirroring what `ai create`
// records) so it is a real scope.IsProjectRoot directory.
func seedProject(test *testing.T, parent, name string) string {
	test.Helper()
	root := filepath.Join(parent, name)
	if err := state.OpenStore(root).SaveProject(&state.Project{Name: name, OS: "debian-trixie", Created: "t"}); err != nil {
		test.Fatal(err)
	}
	return root
}

// enterLocation types path into the location field and presses enter, returning whether
// the step advanced (a valid, created location).
func enterLocation(step *locationStep, path string) bool {
	step.input.SetValue(path)
	advance, _ := step.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return advance
}

func TestLocationStartsInGivenDirectory(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	start := test.TempDir()
	step := newLocationStep(start)
	if got, want := step.input.Value(), ensureTrailingSep(start); got != want {
		test.Fatalf("location field starts at %q, want the given directory %q", got, want)
	}
}

func TestLocationCreatesMissingDirsAndConfirms(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	target := filepath.Join(base, "a", "b", "fresh") // does not exist yet

	step := newLocationStep(base)
	if !enterLocation(step, target) {
		test.Fatalf("a valid new path must advance; err = %v", step.validationErr)
	}
	if step.dir != target {
		test.Fatalf("confirmed dir = %q, want %q", step.dir, target)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		test.Fatalf("the entered path and intermediate folders must be created: %v", err)
	}
}

func TestLocationExpandsTilde(test *testing.T) {
	home := test.TempDir()
	test.Setenv("HOME", home)
	step := newLocationStep(test.TempDir())
	if !enterLocation(step, "~/proj/x") {
		test.Fatalf("expected advance; err = %v", step.validationErr)
	}
	if want := filepath.Join(home, "proj", "x"); step.dir != want {
		test.Fatalf("~ expanded to %q, want %q", step.dir, want)
	}
}

func TestLocationRejectsExistingWorkspaceRoot(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	root := seedProject(test, base, "app")

	step := newLocationStep(base)
	if enterLocation(step, root) {
		test.Fatal("an existing workspace root must not advance")
	}
	if step.validationErr == nil {
		test.Fatal("an existing workspace root must set a validation error")
	}
	if !strings.Contains(step.View(), "already a workspace") {
		test.Errorf("the validation error should show inline, got:\n%s", step.View())
	}
}

func TestLocationRejectsPathNestedInsideWorkspace(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	root := seedProject(test, base, "app")
	nested := filepath.Join(root, "sub", "dir")

	step := newLocationStep(base)
	if enterLocation(step, nested) {
		test.Fatal("a path nested inside a workspace must not advance")
	}
	if step.validationErr == nil {
		test.Fatal("a nested path must set a validation error")
	}
	if !strings.Contains(step.View(), "inside an existing workspace") {
		test.Errorf("the inline error should name the nesting, got:\n%s", step.View())
	}
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		test.Errorf("a rejected nested path must not be created (stat err = %v)", err)
	}
}

func TestLocationDropdownGreysOutWorkspaceFolders(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	seedProject(test, base, "hasws")
	if err := os.MkdirAll(filepath.Join(base, "plain"), 0o755); err != nil {
		test.Fatal(err)
	}

	step := newLocationStep(base)
	var hasWorkspace, plain *folderRow
	for index := range step.suggestions {
		switch step.suggestions[index].name {
		case "hasws":
			hasWorkspace = &step.suggestions[index]
		case "plain":
			plain = &step.suggestions[index]
		}
	}
	if hasWorkspace == nil || plain == nil {
		test.Fatalf("expected both folders in the dropdown, got %+v", step.suggestions)
	}
	if !hasWorkspace.disabled {
		test.Error("a folder that is already a workspace must be disabled")
	}
	if plain.disabled {
		test.Error("a plain folder must be selectable")
	}
	// ↓ from the input row skips the disabled workspace folder and lands on "plain".
	step.Update(tea.KeyMsg{Type: tea.KeyDown})
	if step.selected < 0 || step.suggestions[step.selected].disabled {
		test.Fatalf("↓ must land on a selectable folder, selected=%d", step.selected)
	}
	if step.suggestions[step.selected].name != "plain" {
		test.Errorf("↓ should skip the workspace folder to 'plain', got %q", step.suggestions[step.selected].name)
	}
}

func TestLocationAutocompletionPrefixMatchesChildDirs(test *testing.T) {
	base := test.TempDir()
	test.Setenv("HOME", test.TempDir())
	for _, name := range []string{"projects", "photos"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			test.Fatal(err)
		}
	}
	suggestions := dirSuggestions(filepath.Join(base, "pro"))
	wantProjects := ensureTrailingSep(filepath.Join(base, "projects"))
	found := false
	for _, suggestion := range suggestions {
		if suggestion.path == wantProjects {
			found = true
		}
		if suggestion.name == "photos" {
			test.Errorf("prefix 'pro' must not suggest photos/: %+v", suggestions)
		}
	}
	if !found {
		test.Errorf("prefix 'pro' should suggest projects/, got %+v", suggestions)
	}
}

// TestCreateWizardCancels verifies esc from the first step cancels the whole wizard.
func TestCreateWizardCancels(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	wizard := NewCreate(test.TempDir(), nil, 24, 18)
	cmd := wizard.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		test.Fatal("esc must emit a cancel command")
	}
	if _, ok := cmd().(CreateCancelledMsg); !ok {
		test.Fatalf("want CreateCancelledMsg, got %#v", cmd())
	}
}

// TestCreateWizardAssemblesSpec walks every step and asserts the emitted spec.
func TestCreateWizardAssemblesSpec(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	target := filepath.Join(base, "demo-ws")
	library := []ollama.LibraryModel{{Name: "llama3.2", Tags: []ollama.LibraryTag{{Name: "3b"}, {Name: "latest"}}}}

	wizard := NewCreate(base, library, 24, 18)
	wizard.SetSize(80, 24)

	enter := func() tea.Cmd { return wizard.Update(tea.KeyMsg{Type: tea.KeyEnter}) }

	// Step 1: location → the fresh target path.
	wizard.location.input.SetValue(target)
	if cmd := enter(); cmd != nil {
		test.Fatalf("location step should just advance, got a command: %#v", cmd())
	}
	if wizard.step != stepName {
		test.Fatalf("after location, step = %d, want stepName", wizard.step)
	}
	// Step 2: name.
	wizard.name.input.SetValue("demo-ws")
	enter()
	// Step 3: OS (default debian-trixie). Step 4: shell (default bash). Step 5: agents.
	enter() // OS
	if wizard.step != stepShell {
		test.Fatalf("after OS, step = %d, want stepShell", wizard.step)
	}
	enter() // shell (default bash)
	if wizard.step != stepAgents {
		test.Fatalf("after shell, step = %d, want stepAgents", wizard.step)
	}
	enter() // agents + apps combined (defaults satisfy the ≥1 requirement)
	enter() // default agent
	enter() // stacks (none)
	enter() // cpus (blank)
	enter() // memory (blank)
	enter() // ports (blank)
	enter() // idle (blank) → advances to caveman step
	if wizard.step != stepCaveman {
		test.Fatalf("after idle, step = %d, want stepCaveman", wizard.step)
	}
	enter() // caveman (default install) → advances to model step
	if wizard.step != stepModel {
		test.Fatalf("after caveman, step = %d, want stepModel", wizard.step)
	}
	// Model step: "(none)" is the first row — enter selects it and finishes.
	cmd := enter()
	if cmd == nil {
		test.Fatal("selecting the model (none) should finish and emit a command")
	}
	confirmed, ok := cmd().(CreateConfirmedMsg)
	if !ok {
		test.Fatalf("want CreateConfirmedMsg, got %#v", cmd())
	}
	spec := confirmed.Spec
	if spec.Name != "demo-ws" {
		test.Errorf("spec.Name = %q, want demo-ws", spec.Name)
	}
	if spec.OS != "debian-trixie" {
		test.Errorf("spec.OS = %q, want debian-trixie", spec.OS)
	}
	if spec.Shell != "bash" {
		test.Errorf("spec.Shell = %q, want bash (default)", spec.Shell)
	}
	if spec.Root != target {
		test.Errorf("spec.Root = %q, want %q", spec.Root, target)
	}
	if strings.Join(spec.AgentCLIs, ",") != "opencode,pi" {
		test.Errorf("spec.AgentCLIs = %v, want [opencode pi]", spec.AgentCLIs)
	}
	if spec.DefaultTool != "opencode" {
		test.Errorf("spec.DefaultTool = %q, want opencode", spec.DefaultTool)
	}
	if spec.GraphifyModel != "" {
		test.Errorf("spec.GraphifyModel = %q, want empty (none selected)", spec.GraphifyModel)
	}
	if !spec.CavemanEnabled {
		test.Error("spec.CavemanEnabled = false, want true (default install)")
	}
}

// TestCreateWizardModelDrillDown selects a specific model tag and checks the ref.
func TestCreateWizardModelDrillDown(test *testing.T) {
	library := []ollama.LibraryModel{{Name: "qwen2.5-coder", Tags: []ollama.LibraryTag{{Name: "7b"}, {Name: "3b"}}}}
	picker := newModelPicker(library, "")
	picker.SetSize(80, 20)

	// Move off "(none)" to the model, drill in, pick the first tag.
	picker.Update(tea.KeyMsg{Type: tea.KeyDown})          // (none) -> qwen2.5-coder
	picker.Update(tea.KeyMsg{Type: tea.KeyEnter})         // drill into tags
	done := picker.Update(tea.KeyMsg{Type: tea.KeyEnter}) // select first tag
	if !done {
		test.Fatal("selecting a tag should finish the model step")
	}
	if picker.Value() != "qwen2.5-coder:7b" {
		test.Errorf("picker.Value() = %q, want qwen2.5-coder:7b", picker.Value())
	}
}

// TestLocationAutocompletesOnMove verifies moving the folder selection autocompletes the
// highlighted folder into the input, and moving back above the list restores what was typed.
func TestLocationAutocompletesOnMove(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			test.Fatal(err)
		}
	}
	step := newLocationStep(base)
	step.input.SetValue(ensureTrailingSep(base)) // list the children of base
	step.typed = step.input.Value()
	step.refreshSuggestions()
	step.selected = -1
	if len(step.suggestions) == 0 {
		test.Fatal("expected child-folder suggestions for base")
	}

	// Moving down autocompletes the highlighted folder's path into the input.
	step.moveSelection(1)
	first := step.suggestions[step.selected].path
	if step.input.Value() != first {
		test.Errorf("moving down should autocomplete the folder into the input: got %q, want %q", step.input.Value(), first)
	}
	// Moving back above the list restores the typed prefix.
	step.moveSelection(-1)
	if step.selected != -1 {
		test.Fatalf("moving up past the top should clear the selection, got %d", step.selected)
	}
	if step.input.Value() != ensureTrailingSep(base) {
		test.Errorf("moving above the list should restore the typed prefix: got %q, want %q", step.input.Value(), ensureTrailingSep(base))
	}
}

// TestCreateWizardAuthModeStep drives the wizard selecting the OAuth-capable claude-code,
// confirming an auth-mode step appears for it and populates Spec.AuthModes.
func TestCreateWizardAuthModeStep(test *testing.T) {
	test.Setenv("HOME", test.TempDir())
	base := test.TempDir()
	target := filepath.Join(base, "demo-ws")

	wizard := NewCreate(base, nil, 24, 18) // nil library → the model step auto-advances
	wizard.SetSize(80, 24)
	enter := func() tea.Cmd { return wizard.Update(tea.KeyMsg{Type: tea.KeyEnter}) }
	press := func(k tea.KeyType) { wizard.Update(tea.KeyMsg{Type: k}) }
	space := func() { wizard.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}) }

	wizard.location.input.SetValue(target)
	enter() // location
	wizard.name.input.SetValue("demo-ws")
	enter() // name
	enter() // OS
	enter() // shell
	// agents: add claude-code (index 3) to the opencode+pi defaults.
	for index := 0; index < 3; index++ {
		press(tea.KeyDown)
	}
	space() // toggle claude-code on
	enter() // agents + apps combined
	enter() // default agent
	enter() // stacks
	enter() // cpus
	enter() // memory
	enter() // ports
	enter() // idle → caveman
	enter() // caveman → model
	if wizard.step != stepModel {
		test.Fatalf("expected the model step, got step %d", wizard.step)
	}
	enter() // model (nil library) → auth phase
	if wizard.step != stepAuth {
		test.Fatalf("expected the auth step for the selected claude-code, got step %d", wizard.step)
	}
	if len(wizard.authAgents) != 1 || wizard.authAgents[0] != "claude-code" {
		test.Fatalf("authAgents = %v, want [claude-code]", wizard.authAgents)
	}
	// Pick oauth (the second option) and finish.
	press(tea.KeyDown)
	cmd := enter()
	if cmd == nil {
		test.Fatal("selecting the auth mode should finish and emit a command")
	}
	confirmed, ok := cmd().(CreateConfirmedMsg)
	if !ok {
		test.Fatalf("want CreateConfirmedMsg, got %#v", cmd())
	}
	if confirmed.Spec.AuthModes["claude-code"] != "oauth" {
		test.Errorf("Spec.AuthModes = %v, want claude-code=oauth", confirmed.Spec.AuthModes)
	}
}
