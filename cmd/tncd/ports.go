package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ben-kuhn/tncd/v2/internal/ports"
)

// runPorts implements `tncd ports [--json]`: list serial (and, later, Bluetooth)
// devices with a stable reference to put in the config `device =` key.
func runPorts(args []string) int {
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ps, err := ports.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out, err := formatPorts(ps, *jsonOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// formatPorts renders the port list as an aligned table or as JSON. It is a
// pure function (no I/O) so it can be unit-tested.
func formatPorts(ps []ports.Port, jsonOut bool) (string, error) {
	if jsonOut {
		b, err := json.MarshalIndent(ps, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	}
	if len(ps) == 0 {
		return "no serial ports found\n", nil
	}
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tREF\tLABEL")
	for _, p := range ps {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Device, p.Ref, p.Label)
	}
	tw.Flush()
	return sb.String(), nil
}
