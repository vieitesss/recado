package main

import (
	"context"
	"encoding/json"
	osexec "os/exec"
	"time"

	"github.com/vieitesss/recado/internal/store"
)

// rexTimeout bounds the one rex call a listing makes, so a wedged rex cannot
// hang recado.
const rexTimeout = 3 * time.Second

// rexSessions is the part of `rex ls --json` recado reads.
type rexSessions struct {
	Sessions []struct {
		SessionID string `json:"session_id"`
		Label     string `json:"label"`
	} `json:"sessions"`
}

// rexLabels maps the Rex session ids in a listing to their current labels.
// A label can be renamed after registration, so it is resolved when listing,
// not when the entry was written. Any failure is silent: the label is simply
// unknown.
func (a *app) rexLabels(list []store.Agent) map[string]string {
	need := false
	for _, ag := range list {
		if ag.RexSession != "" {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	data, err := a.rexListOutput()
	if err != nil {
		return nil
	}
	return parseRexSessions(data)
}

// rexListOutput is `rex ls --json`, or the test stub.
func (a *app) rexListOutput() ([]byte, error) {
	if a.rexList != nil {
		return a.rexList()
	}
	path, err := osexec.LookPath("rex")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rexTimeout)
	defer cancel()
	return osexec.CommandContext(ctx, path, "ls", "--json").Output()
}

// parseRexSessions reads session ids and labels out of `rex ls --json`.
func parseRexSessions(data []byte) map[string]string {
	var doc rexSessions
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	labels := make(map[string]string, len(doc.Sessions))
	for _, s := range doc.Sessions {
		if s.SessionID != "" && s.Label != "" {
			labels[s.SessionID] = s.Label
		}
	}
	return labels
}
