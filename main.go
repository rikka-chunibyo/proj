package main

import (
	"fmt"
	"os"
)

func main() {
	if helpHandled(os.Args[1:]) {
		return
	}
	app, err := newApp()
	if err == nil {
		err = app.Run(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "proj: %v\n", err)
		os.Exit(1)
	}
}

func helpHandled(args []string) bool {
	if len(args) == 0 || len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		printHelp(os.Stdout)
		return true
	}
	if len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version") {
		fmt.Fprintf(os.Stdout, "proj %s\n", version)
		return true
	}
	if len(args) == 2 && args[1] == "--help" {
		dummy := &App{Out: os.Stdout, Err: os.Stderr}
		if err := dummy.Run(args); err == nil {
			return true
		}
	}
	return false
}
