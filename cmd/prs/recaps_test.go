package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/datapointchris/tuidrive"
)

// withRecaps puts a stand-in recap program on PATH and names its three commands in the
// config. list prints the list file, show prints a window's file, and write takes a second,
// then puts its window atop the list and writes its file, as a recap written on demand joins
// the list. Each records its arguments beside the other stand-ins'.
func (s standIn) withRecaps(t *testing.T) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(s.config), "..", "..", "recaps")
	files := map[string]string{
		"list":              "day 2026-11-06\nweek 2026-W45\n",
		"day-2026-11-06.md": "# Recap of 2026-11-06\n\nThe sixth went quietly.\n",
		"week-2026-W45.md":  "# Recap of 2026-W45\n\nWeek forty-five landed the tab.\n",
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" + fmt.Sprintf("printf '%%s\\n' \"$(basename \"$0\")\" \"cwd=$(pwd)\" \"$@\" >> %q\n", s.recorded) +
		fmt.Sprintf("dir=%q\n", dir) + `case "$1" in
list) cat "$dir/list" ;;
show) [ -f "$dir/${2#--}-$3.md" ] && exec cat "$dir/${2#--}-$3.md"; echo "no recap of ${2#--} $3" >&2; exit 1 ;;
write) sleep 1; printf '%s %s\n' "${2#--}" "$3" | cat - "$dir/list" > "$dir/list.new" && mv "$dir/list.new" "$dir/list"
  printf '# Recap of %s\n\nWritten on demand.\n' "$3" > "$dir/${2#--}-$3.md" ;;
esac
`
	bin := strings.SplitN(s.env["PATH"], string(os.PathListSeparator), 2)[0]
	if err := os.WriteFile(filepath.Join(bin, "recap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	config, err := os.OpenFile(s.config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = config.Close() }()
	if _, err := config.WriteString(`recaps:
  needs: [recap]
  list: [recap, list]
  show: [recap, show, "--{kind}", "{label}"]
  write: [recap, write, "--{kind}", "{label}"]
`); err != nil {
		t.Fatal(err)
	}
}

// recordedRuns counts the stand-in recap's runs whose arguments are args.
func (s standIn) recordedRuns(t *testing.T, args ...string) int {
	t.Helper()
	recorded, err := os.ReadFile(s.recorded)
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = regexp.QuoteMeta(arg)
	}
	return len(regexp.MustCompile(`(?m)^recap\ncwd=[^\n]*\n`+strings.Join(quoted, `\n`)+`\n`).FindAllIndex(recorded, -1))
}

// The recaps tab lists each window the list prints, with the cursor's recap beside it. w asks
// naming the model call, and a window typed there that the list does not hold is written,
// then joins the list as it is run again.
func TestTheRecapsTabShowsEachWindowsRecapAndWritesOneAfterAskingThenListsAgain(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	s.env["NO_COLOR"] = "1"
	s.withRecaps(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--view", "recaps"}, Env: s.env, Cols: 200, Rows: 40})
	term.WaitForPattern(`pull-requests\s+summary\s+\[recaps\]`)
	term.WaitFor("RECAPS · 2")
	term.WaitFor("The sixth went quietly.")
	term.Press("j")
	term.WaitFor("Week forty-five landed the tab.")

	term.Press("w")
	term.WaitFor("write the recap of week 2026-W45 with a model call?")
	term.Type("day 2026-11-07")
	term.Press("enter")
	term.WaitFor("writing the recap of day 2026-11-07")
	term.WaitFor("wrote the recap of day 2026-11-07")
	term.WaitFor("RECAPS · 3")
	term.Press("g")
	term.WaitFor("Written on demand.")
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
	if n := s.recordedRuns(t, "write", "--day", "2026-11-07"); n != 1 {
		t.Errorf("the write should run once for the window typed, ran %d times", n)
	}
	if n := s.recordedRuns(t, "list"); n < 2 {
		t.Errorf("the list should run again once the write ends, ran %d times", n)
	}
}

// A config naming no recaps commands draws no recaps tab, and --view recaps says why. Naming
// them, --json prints what its schema holds, with the first window's recap read.
func TestTheRecapsTabIsDrawnOnlyWhereTheConfigNamesItsCommandsAndItsSnapshotHoldsItsSchema(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	s.env["NO_COLOR"] = "1"
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitForPattern(`\[pull-requests\]\s+summary`)
	if strings.Contains(term.Frame().Text(), "recaps") {
		t.Errorf("a config naming no recaps commands drew a recaps tab:\n%s", term.Frame().Text())
	}
	term.Press("q")
	term.WaitExit()
	if _, said, code := s.run(t, "tui", "--view", "recaps", "--json"); code != 1 || !strings.Contains(said, "the config names no recaps commands") {
		t.Errorf("--view recaps with no recaps commands exited %d, saying %q", code, said)
	}

	s.withRecaps(t)
	out, said, code := s.run(t, "tui", "--view", "recaps", "--json")
	if code != 0 {
		t.Fatalf("--view recaps --json exited %d: %s", code, said)
	}
	if err := schemaIn(t, "recaps.schema.json").Validate(decoded(t, out)); err != nil {
		t.Fatalf("--view recaps --json printed what its schema refuses: %v\n%s", err, out)
	}
	var snap struct {
		Windows []struct {
			Kind, Label string
			Recap       *string
		}
	}
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 || snap.Windows[0].Label != "2026-11-06" || snap.Windows[0].Recap == nil ||
		!strings.Contains(*snap.Windows[0].Recap, "The sixth went quietly.") || snap.Windows[1].Recap != nil {
		t.Errorf("the snapshot should list both windows with only the first's recap read, printed %s", out)
	}
}
