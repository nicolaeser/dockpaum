package cli

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/banner"
)

func runPlan(inv *invocation, stdout, stderr io.Writer, now time.Time) int {
	ev, err := evaluate(inv, stderr, now)
	if err != nil {
		return evaluationExit(err)
	}
	doc := planDoc{
		GeneratedAt: now.UTC(), Instance: ev.instance, DryRun: ev.cfg.DryRun,
		Schedule: scheduleSummary(ev.cfg.Schedule), Containers: ev.entries(),
		Unmanaged: ev.unmanaged(), Errors: append([]string{}, ev.errs...),
	}
	if inv.json {
		if err := writeJSON(stdout, doc); err != nil {
			printf(stderr, "plan: %v\n", err)
			return exitFailure
		}
	} else if err := ev.writePlan(stdout, doc, banner.ColorFor(stdout)); err != nil {
		printf(stderr, "plan: %v\n", err)
		return exitFailure
	}
	return ev.finish(stderr, doc.Containers)
}

func runExplain(inv *invocation, stdout, stderr io.Writer, now time.Time) int {
	ev, err := evaluate(inv, stderr, now)
	if err != nil {
		return evaluationExit(err)
	}
	items := ev.explain(inv.plan.target)
	if inv.json {
		doc := explainDoc{
			GeneratedAt: now.UTC(), Instance: ev.instance, DryRun: ev.cfg.DryRun,
			Containers: items, Errors: append([]string{}, ev.errs...),
		}
		if doc.Containers == nil {
			doc.Containers = []explained{}
		}
		if err := writeJSON(stdout, doc); err != nil {
			printf(stderr, "explain: %v\n", err)
			return exitFailure
		}
	} else if err := ev.writeExplain(stdout, items, banner.ColorFor(stdout)); err != nil {
		printf(stderr, "explain: %v\n", err)
		return exitFailure
	}
	entries := make([]planEntry, 0, len(items))
	for _, it := range items {
		entries = append(entries, it.planEntry)
	}
	return ev.finish(stderr, entries)
}

func evaluationExit(err error) int {
	var ee errEvaluation
	if errors.As(err, &ee) {
		return ee.code
	}
	return exitFailure
}

func (ev *evaluation) finish(stderr io.Writer, entries []planEntry) int {
	failed := 0
	for _, e := range entries {
		if e.Action == actionError {
			failed++
		}
	}
	for _, e := range ev.errs {
		printf(stderr, "error: %s\n", cell(e))
	}
	if failed > 0 {
		printf(stderr, "error: %d container%s could not be checked (see the reason)\n", failed, plural(failed))
	}
	if failed > 0 || len(ev.errs) > 0 {
		return exitFailure
	}
	return exitOK
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func actionStyle(action string) string {
	switch action {
	case actionUpdate:
		return styleGreen
	case actionReport:
		return styleCyan
	case actionWait, actionUnknown:
		return styleYellow
	case actionError:
		return styleRed
	case actionHold, actionNone:
		return styleDim
	}
	return ""
}

func (ev *evaluation) writePlan(w io.Writer, doc planDoc, color bool) error {
	mode := "applies updates"
	if doc.DryRun {
		mode = "dry_run: reports updates, changes nothing"
	}
	printf(w, "%s\n\n", paint("dockpaum plan for "+doc.Instance+": "+mode+", "+doc.Schedule, styleBold, color))
	if len(doc.Containers) == 0 {
		printf(w, "no managed container matches\n")
	} else {
		rows := make([][]string, 0, len(doc.Containers))
		for _, e := range doc.Containers {
			rows = append(rows, []string{
				cell(e.Host), cell(e.Container), cell(policyLabelText(e)), cell(changeText(e)),
				cell(e.Action), cell(ev.whenText(e)), cell(e.Reason),
			})
		}
		header := []string{"HOST", "CONTAINER", "POLICY", "CURRENT -> CANDIDATE", "PLAN", "WHEN", "REASON"}
		err := writeTable(w, header, rows, color, func(row, col int) string {
			switch {
			case row < 0:
				return styleBold
			case col == 4:
				return actionStyle(doc.Containers[row].Action)
			}
			return ""
		})
		if err != nil {
			return err
		}
	}
	if n := len(doc.Unmanaged); n > 0 {
		if ev.scoped {
			printf(w, "\n")
			for _, u := range doc.Unmanaged {
				printf(w, "%s on %s is not managed: %s\n", cell(u.Container), cell(u.Host), cell(u.Reason))
			}
		} else {
			printf(w, "\n%d other container%s not managed by this instance (filters); dockpaum explain <container> says why\n", n, pluralVerb(n))
		}
	}
	return nil
}

func pluralVerb(n int) string {
	if n == 1 {
		return " is"
	}
	return "s are"
}

func verdictStyle(v string) string {
	switch v {
	case verdictPass:
		return styleGreen
	case verdictStop:
		return styleRed
	case verdictAtUpdate:
		return styleCyan
	}
	return styleDim
}

func (ev *evaluation) writeExplain(w io.Writer, items []explained, color bool) error {
	for i, it := range items {
		if i > 0 {
			printf(w, "\n")
		}
		printf(w, "%s\n", paint(it.Container+" on "+it.Host, styleBold, color))
		facts := [][]string{
			{"image", cell(it.Image)},
			{"policy", cell(policyLabelText(it.planEntry))},
			{"mode", cell(orDash(it.Mode))},
			{"current", cell(imageText(it.Current, ""))},
		}
		if it.Candidate != nil {
			facts = append(facts, []string{"candidate", cell(imageText(*it.Candidate, it.Current.Tag))})
		}
		facts = append(facts,
			[]string{"plan", paint(it.Action, actionStyle(it.Action), color) + ": " + cell(it.Reason)},
			[]string{"when", cell(ev.whenText(it.planEntry))},
		)
		for _, f := range facts {
			printf(w, "  %-10s %s\n", f[0], f[1])
		}
		printf(w, "\nGates, in the order the daemon applies them:\n")
		rows := make([][]string, 0, len(it.Gates))
		for _, g := range it.Gates {
			rows = append(rows, []string{"  " + g.Gate, g.Verdict, cell(orDash(g.Detail))})
		}
		err := writeTable(w, []string{"  GATE", "VERDICT", "DETAIL"}, rows, color, func(row, col int) string {
			switch {
			case row < 0:
				return styleBold
			case col == 1:
				return verdictStyle(it.Gates[row].Verdict)
			}
			return ""
		})
		if err != nil {
			return err
		}
		var changes []string
		for _, g := range it.Gates {
			if g.Change != "" {
				changes = append(changes, g.Gate+": "+g.Change)
			}
		}
		if len(changes) > 0 {
			printf(w, "\nWhat would change it:\n")
			for _, c := range changes {
				printf(w, "  %s\n", cell(c))
			}
		}
	}
	if len(items) == 0 {
		printf(w, "nothing to explain\n")
	}
	return nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
