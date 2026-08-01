package web

// The failure ladder's FINAL reload - the one after a successful blanket
// rollback of the pre-import login/access keys - can return
// settings.ErrLegacyReseal. That sentinel means the reload SUCCEEDED past the
// broadcast: the rolled-back config is running, and only re-encrypting the
// backup's clear-text iperf3 passwords failed, leaving them on disk in the
// clear. Lumping it in with "reload failed" told the operator the opposite of
// the truth twice over: an active config was reported as waiting for a
// restart, and the real problem - unencrypted credentials at rest - was
// neither warned about nor logged. The ladder's OTHER reloads already
// special-case the sentinel; this pins the same honesty onto the last one.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// sealFailCrypter makes every Seal fail, so any reload that finds legacy
// clear-text passwords ends in ErrLegacyReseal. Unseal passes through; the
// legacy values never reach it (unsealServers spots clear-text first).
type sealFailCrypter struct{}

func (sealFailCrypter) Seal(string) (string, error)     { return "", errors.New("seal unavailable") }
func (sealFailCrypter) Unseal(s string) (string, error) { return s, nil }

// newResealFailServer is newTestServerLog with encryption on but broken - the
// combination that turns a reload over legacy plaintext into ErrLegacyReseal.
func newResealFailServer(t *testing.T, logDst *bytes.Buffer) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2,
	}, settings.WithCrypter(sealFailCrypter{}))
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	return New(st, nil, nil, set, nil, "test", slog.New(slog.NewTextHandler(logDst, nil)))
}

func TestRolledBackConfigLeftLiveByAResealFailureIsReportedAsLive(t *testing.T) {
	var logBuf bytes.Buffer
	s := newResealFailServer(t, &logBuf)
	exhaustReconcileBudget(t) // both budgeted reloads fail -> the blanket rollback runs

	// An older export: a latency change plus an iperf3 server whose password
	// still rides in the clear (mergeImportedIperfPasswords keeps it). The final
	// reload after the rollback broadcasts this config, then fails its re-seal.
	rr := importConfig(t, s,
		`{"key":"latency_interval_s","value":"9"},`+
			`{"key":"iperf_servers","value":"[{\"label\":\"NAS\",\"addr\":\"10.0.0.5:5201\",\"auth\":true,\"password\":\"PLAINTEXT\"}]"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	// Premise, not the assertion: ErrLegacyReseal fails AFTER the broadcast, so
	// the imported config IS running when the response is written.
	if got := s.settings.LatencyInterval(); got != 9*time.Second {
		t.Fatalf("fixture: imported latency is not live (got %v) - the final reload did not broadcast", got)
	}
	warnings := strings.Join(warningsOf(t, rr), "\n")
	if warnings == "" {
		t.Fatal("no warnings at all: a rollback that left unencrypted credentials on disk reported a clean import")
	}
	if strings.Contains(warnings, "takes effect at the next restart") {
		t.Errorf("the response says the config is waiting for a restart, but it is already live:\n%s", warnings)
	}
	if !strings.Contains(warnings, "encrypt") {
		t.Errorf("the response never mentions the actual failure - iperf3 passwords left unencrypted on disk:\n%s", warnings)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "re-encryption failed") {
		t.Errorf("the reseal failure was never logged; logs:\n%s", logs)
	}
}
