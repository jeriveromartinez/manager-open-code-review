package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

func usage() string {
	return "usage: qa <serve|worker|migrate>"
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe()
	case "worker":
		err = runWorker()
	case "migrate":
		err = runMigrate()
	default:
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}

	if err != nil {
		logger.Error("command failed", "command", os.Args[1], "error", err)
		os.Exit(1)
	}
}

func runServe() error {
	return errors.New("not implemented")
}

func runWorker() error {
	return errors.New("not implemented")
}

func runMigrate() error {
	return errors.New("not implemented")
}
