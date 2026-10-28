// meowcaller_init.go — Initialize meowcaller client at startup
//
// This file provides the hook that main.go calls after hypermeow client
// is created but BEFORE Connect() — meowcaller requires its handlers to be
// installed before the receive loop starts.

package main

import (
	whatsmeow "github.com/polymorfa/hypermeow"
)

// initMeowcallerIfEnabled is called from main.go after creating the hypermeow client.
// Always active (meowcaller is the only call mode in v2.10+).
func initMeowcallerIfEnabled(wa *whatsmeow.Client) {
	initMeowcallerClient(wa)
}
