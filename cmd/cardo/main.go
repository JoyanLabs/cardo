// Command cardo swaps which engine holds one GPU slot: drain first, roll
// back if the new one does not answer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/state"
	"github.com/JoyanLabs/cardo/internal/swap"
)

// defaultConfDir is $XDG_CONFIG_HOME/cardo/engines.d (default
// ~/.config/cardo/engines.d): one file per engine, no hardcoded paths.
func defaultConfDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "cardo", "engines.d")
}

const usage = `cardo — one model per GPU. Swap which engine holds the slot.

Usage:
  cardo [--conf-dir DIR] list          registered engines and which one is up
  cardo [--conf-dir DIR] status        what the state record says
  cardo [--conf-dir DIR] up NAME      drain, stop, start, verify, roll back on failure
  cardo [--conf-dir DIR] down         stop the resident engine, draining first

Engines are declared in small .conf files, one per engine, under --conf-dir
(default %s).`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("cardo", flag.ContinueOnError)
	confDir := fs.String("conf-dir", "", "directory holding one .conf per engine")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), usage+"\n", defaultConfDir())
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	if *confDir == "" {
		*confDir = defaultConfDir()
	}
	st := &state.Store{Dir: state.ResolveDir("")}
	var err error

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// status speaks only to the state record; the other verbs need engines.
	var reg *config.Registry
	switch fs.Arg(0) {
	case "status":
		return doStatus(st)
	case "list", "up", "down":
		r, err := config.LoadDir(*confDir)
		if errors.Is(err, os.ErrNotExist) {
			if fs.Arg(0) == "list" {
				fmt.Fprintf(os.Stderr, "cardo: no engines: %s does not exist yet\n", *confDir)
				return 0
			}
			fmt.Fprintln(os.Stderr, "cardo:", err)
			return 1
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "cardo:", err)
			return 1
		}
		reg = r
	default:
		fmt.Fprintf(os.Stderr, "cardo: unknown verb %q (want list, status, up, down)\n", fs.Arg(0))
		return 2
	}

	sw := &swap.Swapper{Registry: reg, State: st, Out: os.Stderr}

	switch verb := fs.Arg(0); verb {
	case "list":
		return doList(reg, st)
	case "up":
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "cardo: up takes exactly one engine name")
			return 2
		}
		err = sw.Up(ctx, fs.Arg(1))
	case "down":
		err = sw.Down(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	return 0
}

func doList(reg *config.Registry, st *state.Store) int {
	rec, err := st.Load()
	active := ""
	if err == nil {
		active = rec.Active
	} else if !errors.Is(err, state.ErrNoState) {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	names := reg.Names()
	if len(names) == 0 {
		fmt.Println("no engines registered under the config dir")
		return 0
	}
	for _, n := range names {
		e, _ := reg.Get(n)
		mark := " "
		if n == active {
			mark = "*"
		}
		desc := ""
		if e.Description != "" {
			desc = "  — " + e.Description
		}
		fmt.Printf("%s %-24s %-8s slot %d%s\n", mark, n, e.Type, e.Slot, desc)
	}
	return 0
}

func doStatus(st *state.Store) int {
	rec, err := st.Load()
	if errors.Is(err, state.ErrNoState) {
		fmt.Println("nothing resident (no state record)")
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	if rec.Active == "" {
		fmt.Println("nothing resident")
	} else {
		fmt.Printf("active:   %s (since %s)\n", rec.Active, rec.ActiveAt.Format("2006-01-02 15:04:05"))
	}
	if rec.Previous != "" {
		fmt.Printf("previous: %s (rollback target)\n", rec.Previous)
	}
	return 0
}
