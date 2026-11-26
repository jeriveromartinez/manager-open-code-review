// Command fakeocr is a platform-independent stand-in for the OCR binary in
// resume tests. It reads its answer from JSON files and can block on a chosen
// chunk, which lets a test interrupt the run deterministically.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	dir := os.Getenv("RUN_FAKE_OCR_DIR")
	phase := os.Getenv("RUN_FAKE_OCR_PHASE")
	if phase == "" {
		phase = "0"
	}
	appendLine(os.Getenv("RUN_FAKE_OCR_CALLS"), "call\n")
	appendLine(os.Getenv("RUN_FAKE_OCR_ARGV"), strings.Join(os.Args[1:], " ")+"\n")
	if session := os.Getenv("RUN_FAKE_OCR_EMIT_SESSION"); session != "" {
		fmt.Fprintf(os.Stderr, "[ocr] Session: %s (retry with: --resume %s)\n", session, session)
	}
	if argValue(os.Args[1:], "--resume") != "" && os.Getenv("RUN_FAKE_OCR_FAIL_RESUME") == "1" {
		fmt.Fprintln(os.Stderr, "ocr review: session not found")
		os.Exit(1)
	}

	commit := argValue(os.Args[1:], "--commit")
	if commit == "" {
		if phase == "0" && os.Getenv("RUN_FAKE_OCR_HANG_MAIN") == "1" {
			writeFile(os.Getenv("RUN_FAKE_OCR_READY"), "ready")
			time.Sleep(time.Minute)
			return
		}
		output := os.Getenv("RUN_FAKE_OCR_MAIN_OUTPUT")
		if output == "" {
			output = filepath.Join(dir, "main.json")
		}
		// The analysis hang flushes the report only when the worker asks OCR to
		// close, which is how a deadline close preserves the review outcome. It
		// fires only for the analysis phase, so the review invocation can finish
		// first.
		if phase == "0" && os.Getenv("RUN_FAKE_OCR_ANALYSIS_HANG") == "1" &&
			hasArg(os.Args[1:], "--analysis") {
			writeFile(os.Getenv("RUN_FAKE_OCR_READY"), "ready")
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, os.Interrupt)
			select {
			case <-signals:
				printFile(output)
				os.Exit(1)
			case <-time.After(time.Minute):
			}
			return
		}
		printFile(output)
		return
	}

	counterPath := os.Getenv("RUN_FAKE_OCR_COUNTER")
	n := 0
	if raw, err := os.ReadFile(counterPath); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	n++
	writeFile(counterPath, strconv.Itoa(n))

	if phase == "0" {
		if hang, _ := strconv.Atoi(os.Getenv("RUN_FAKE_OCR_HANG_CHUNK")); hang > 0 && n == hang {
			writeFile(os.Getenv("RUN_FAKE_OCR_READY"), "ready")
			time.Sleep(time.Minute)
			return
		}
	}
	output := filepath.Join(dir, "phase"+phase, fmt.Sprintf("chunk%d.json", n))
	if _, err := os.Stat(output); err != nil {
		output = filepath.Join(dir, fmt.Sprintf("chunk%d.json", n))
	}
	printFile(output)
}

func argValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func printFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeocr: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(raw)
}

func appendLine(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

func writeFile(path, content string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(content), 0o644)
}
