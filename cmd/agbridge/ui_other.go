//go:build !windows

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// Outside Windows agbridge runs headless (development and tests).
var shimBinary []byte

const isGUI = false

func runUI(a *app, _ bool) {
	log.Printf("console: %s", a.consoleURL())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func openBrowser(url string) { log.Printf("open %s", url) }

func fail(msg string) {
	log.Print(msg)
	os.Exit(1)
}
