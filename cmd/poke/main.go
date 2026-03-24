package main

import (
	"fmt"
	"os"

	"github.com/kacperkwapisz/poke-go/internal/auth"
	"github.com/kacperkwapisz/poke-go/internal/tunnel"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "login":
		if err := auth.Login(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "logout":
		if err := auth.Logout(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "tunnel":
		if err := tunnel.Run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`poke ` + version + ` — connect local servers to Poke

Usage:
  poke login                           authenticate with poke.com
  poke logout                          remove stored credentials
  poke tunnel <url> [--name <name>]    expose a local server via poke tunnel
  poke version                         print version

Examples:
  poke login
  poke tunnel http://localhost:3000/mcp --name my-server
  poke logout
`)
}
