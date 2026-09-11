package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestCodexWebsocketLifecycleLogsOmitSensitiveValues(t *testing.T) {
	previousLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previousLevel)
	})

	const (
		sentinelSession = "sess-SENSITIVE-SESSION-ID"
		sentinelAuth    = "auth-SENSITIVE-AUTH-ID"
		sentinelURL     = "wss://chatgpt.com/backend-api/codex?access_token=SENSITIVE-TOKEN"
		sentinelReason  = "provider-reason-SENSITIVE-REASON"
		sentinelModel   = "gpt-5.4"
	)
	sentinelErr := errors.New("upstream 401 SENSITIVE-ERROR-DETAIL")

	logCodexWebsocketStreamStart(sentinelModel)
	logCodexWebsocketConnected(sentinelSession, sentinelAuth, sentinelURL)
	logCodexWebsocketDisconnected(sentinelSession, sentinelAuth, sentinelURL, sentinelReason, sentinelErr)
	logCodexWebsocketDisconnected(sentinelSession, sentinelAuth, sentinelURL, sentinelReason, nil)

	entries := hook.AllEntries()
	if len(entries) == 0 {
		t.Fatal("expected Codex websocket lifecycle log entries")
	}

	forbidden := []string{
		sentinelSession,
		sentinelAuth,
		sentinelURL,
		"SENSITIVE-TOKEN",
		sentinelReason,
		"SENSITIVE-ERROR-DETAIL",
	}

	var sawConnected, sawDisconnected, sawStream bool
	for _, entry := range entries {
		formatted, _ := entry.String()
		blob := entry.Message + formatted + fmt.Sprint(entry.Data)
		for _, value := range forbidden {
			if strings.Contains(blob, value) {
				t.Errorf("log leaked %q: %s", value, blob)
			}
		}
		switch {
		case strings.Contains(entry.Message, "upstream connected"):
			sawConnected = true
		case strings.Contains(entry.Message, "upstream disconnected"):
			sawDisconnected = true
		case strings.Contains(entry.Message, "stream request") && strings.Contains(entry.Message, sentinelModel):
			sawStream = true
		}
	}

	if !sawConnected || !sawDisconnected || !sawStream {
		t.Fatalf("missing expected log events connected=%v disconnected=%v stream=%v", sawConnected, sawDisconnected, sawStream)
	}
}
