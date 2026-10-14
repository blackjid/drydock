// Package dyffreport renders manifest diff results with the dyff library
// (https://github.com/homeport/dyff). It runs in-process; no dyff binary is
// executed.
package dyffreport

import (
	"errors"
	"fmt"
	"io"
	"unicode"

	"github.com/gonvenience/bunt"
	"github.com/gonvenience/term"
	"github.com/gonvenience/ytbx"
	"github.com/homeport/dyff/pkg/dyff"
	yamlv3 "go.yaml.in/yaml/v3"

	"github.com/sholdee/drydock/internal/diff"
)

// Write renders one dyff report per result, each preceded by the resource
// header. It compares the result display bodies, so Secret redaction and
// ignore rules already applied by the diff engine are preserved. When dyff
// cannot compare a resource or finds no structural difference although the
// unified diff is non-empty, the resource falls back to its unified diff so a
// real change is never silently dropped.
//
// dyff styles output through process-wide bunt settings, so Write is not safe
// to call concurrently with itself or other bunt users.
func Write(w io.Writer, results []diff.Result, color bool) error {
	// Measure once: dyff re-reads the terminal for every decision, and on some
	// platforms that probes the whole process table.
	width, height := term.GetTerminalSize()
	defer configure(color, width, height)()

	for _, result := range results {
		if err := writeResult(w, result, color); err != nil {
			return err
		}
	}
	return nil
}

// configure sets dyff's process-wide color mode and terminal size and returns
// a function that restores the defaults. True color stays off so output does
// not depend on COLORTERM.
func configure(color bool, width, height int) func() {
	state := bunt.ON
	if !color {
		state = bunt.OFF
	}
	bunt.SetColorSettings(state, bunt.OFF)
	prevWidth, prevHeight := term.FixedTerminalWidth, term.FixedTerminalHeight
	term.FixedTerminalWidth, term.FixedTerminalHeight = width, height
	return func() {
		bunt.SetColorSettings(bunt.AUTO, bunt.AUTO)
		term.FixedTerminalWidth, term.FixedTerminalHeight = prevWidth, prevHeight
	}
}

func writeResult(w io.Writer, result diff.Result, color bool) error {
	header := result.Header()
	if color {
		header = "\x1b[1m" + header + "\x1b[0m"
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}

	report, err := compare(result)
	if err == nil && len(report.Diffs) == 0 {
		err = errors.New("no structural differences found")
	}
	if err != nil {
		_, writeErr := fmt.Fprintf(w, "(dyff: %v; showing unified diff)\n%s", err, result.Diff)
		return writeErr
	}
	human := &dyff.HumanReport{
		Report:                report,
		Indent:                2,
		UseIndentLines:        true,
		MinorChangeThreshold:  0.1,
		MultilineContextLines: 4,
		OmitHeader:            true,
		DoNotInspectCerts:     true,
	}
	return human.WriteReport(w)
}

func compare(result diff.Result) (dyff.Report, error) {
	from, err := inputFile(result.Header(), result.Before)
	if err != nil {
		return dyff.Report{}, fmt.Errorf("load before body: %w", err)
	}
	to, err := inputFile(result.Header(), result.After)
	if err != nil {
		return dyff.Report{}, fmt.Errorf("load after body: %w", err)
	}
	if unsafeDocuments(from.Documents) || unsafeDocuments(to.Documents) {
		return dyff.Report{}, errors.New("values contain control characters")
	}
	// FormatStrings stays off: it round-trips number-like strings through
	// float64 and hides real value changes.
	return dyff.CompareInputFiles(from, to,
		dyff.KubernetesEntityDetection(true),
		dyff.DetectRenames(true),
	)
}

func inputFile(location, body string) (ytbx.InputFile, error) {
	documents, err := ytbx.LoadDocuments([]byte(body))
	if err != nil {
		return ytbx.InputFile{}, err
	}
	return ytbx.InputFile{Location: location, Documents: documents}, nil
}

// unsafeDocuments reports whether any decoded string would print control
// characters or extra lines that the YAML-escaped unified diff keeps inert:
// dyff writes decoded values raw, so a manifest could otherwise move the
// cursor, plant terminal links, or forge `@@` lines in a PR comment. Multi-line
// values and tabs are legitimate; keys must be single-line.
func unsafeDocuments(documents []*yamlv3.Node) bool {
	for _, document := range documents {
		if unsafeNode(document, false) {
			return true
		}
	}
	return false
}

func unsafeNode(node *yamlv3.Node, isKey bool) bool {
	if node == nil || node.Kind == yamlv3.AliasNode {
		return false
	}
	if node.Kind == yamlv3.ScalarNode {
		for _, r := range node.Value {
			if r == '\n' && !isKey || r == '\t' {
				continue
			}
			if unicode.IsControl(r) {
				return true
			}
		}
		return false
	}
	for i, child := range node.Content {
		if unsafeNode(child, node.Kind == yamlv3.MappingNode && i%2 == 0) {
			return true
		}
	}
	return false
}
