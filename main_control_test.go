package main

import (
	"errors"
	"testing"

	"github.com/kardianos/service"
)

// fakeStatusService stubs only Status; the embedded interface satisfies the
// rest (never called by effectiveControlAction).
type fakeStatusService struct {
	service.Service
	st  service.Status
	err error
}

func (f fakeStatusService) Status() (service.Status, error) { return f.st, f.err }

// The restart-of-a-stopped-service downgrade: restart's platform stop half
// fails when nothing is running (Windows: "The service has not been started"),
// stranding the documented Windows upgrade flow. Only a POSITIVE stopped
// status may downgrade - an errored status must leave restart alone so the
// real error (not installed, no permission) surfaces from the restart itself.
func TestEffectiveControlAction(t *testing.T) {
	cases := []struct {
		name   string
		action string
		st     service.Status
		err    error
		want   string
	}{
		{"restart of stopped becomes start", "restart", service.StatusStopped, nil, "start"},
		{"restart of running stays restart", "restart", service.StatusRunning, nil, "restart"},
		{"restart with unknown status stays restart", "restart", service.StatusUnknown, nil, "restart"},
		{"restart with status error stays restart", "restart", service.StatusStopped, errors.New("not installed"), "restart"},
		{"start passes through untouched", "start", service.StatusStopped, nil, "start"},
		{"stop passes through untouched", "stop", service.StatusStopped, nil, "stop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveControlAction(c.action, fakeStatusService{st: c.st, err: c.err})
			if got != c.want {
				t.Fatalf("effectiveControlAction(%q, status=%v err=%v) = %q, want %q", c.action, c.st, c.err, got, c.want)
			}
		})
	}
}
