package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jeriveromartinez/qa-control-center/internal/reviewbench"
)

func main() {
	manifestPath := flag.String("manifest", "", "path to the versioned manifest")
	observationsPath := flag.String("observations", "", "path to a JSON array of observations")
	adjudicationsPath := flag.String("adjudications", "", "optional JSON array of adjudication decisions")
	outPath := flag.String("out", "", "report output path (default stdout)")
	revealPath := flag.String("reveal-out", "", "optional path for the post-adjudication reveal")
	printLock := flag.Bool("print-lock", false, "print the manifest lock and exit")
	flag.Parse()

	if *manifestPath == "" {
		fail("missing -manifest")
	}
	if *printLock {
		printManifestLock(*manifestPath)
		return
	}
	if *observationsPath == "" {
		fail("missing -observations")
	}

	manifest, err := reviewbench.LoadManifest(*manifestPath)
	if err != nil {
		fail("%v", err)
	}
	observations, err := reviewbench.LoadObservations(*observationsPath)
	if err != nil {
		fail("%v", err)
	}
	dataset, err := reviewbench.BuildDataset(manifest, observations)
	if err != nil {
		fail("%v", err)
	}
	sealed, mapping := reviewbench.Seal(dataset)
	var decisions []reviewbench.AdjudicationDecision
	if *adjudicationsPath != "" {
		data, err := os.ReadFile(*adjudicationsPath)
		if err != nil {
			fail("%v", err)
		}
		if err := json.Unmarshal(data, &decisions); err != nil {
			fail("adjudications: %v", err)
		}
	}
	report, err := reviewbench.BuildReport(sealed, decisions)
	if err != nil {
		fail("%v", err)
	}
	if err := writeJSON(*outPath, report); err != nil {
		fail("%v", err)
	}
	if *revealPath != "" {
		revealed, err := reviewbench.Reveal(report, mapping)
		if err != nil {
			fail("%v", err)
		}
		if err := writeJSON(*revealPath, revealed); err != nil {
			fail("%v", err)
		}
	}
}

func printManifestLock(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fail("%v", err)
	}
	var manifest reviewbench.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		fail("%v", err)
	}
	if err := manifest.Validate(); err != nil {
		fail("%v", err)
	}
	lock, err := manifest.ComputeLock()
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(lock)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "reviewbench: "+format+"\n", args...)
	os.Exit(1)
}
