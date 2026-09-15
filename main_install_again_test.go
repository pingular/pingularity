package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/kardianos/service"
)

// A second `install` over a registered service is almost always an upgrade:
// `brew install` or `brew upgrade` put the new binary in place, the caveat said
// to run install, and the service manager answered in its own words ("Init
// already exists: /Library/LaunchDaemons/pingularity.plist"), which say nothing
// about the running service still being the old binary, or how to change that.
func TestInstallOverAnInstalledServiceSaysHowToRunTheNewVersion(t *testing.T) {
	for _, st := range []service.Status{service.StatusRunning, service.StatusStopped, service.StatusUnknown} {
		err := refuseReinstall(fakeStatusService{st: st})
		if err == nil {
			t.Fatalf("install over a service whose status reads %v went ahead; the service manager refuses it with no word on what to do", st)
		}
		for _, want := range []string{"already installed", elevate("pingularity restart"), elevate("pingularity uninstall")} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q:\n%s", want, err)
			}
		}
	}
	if err := refuseReinstall(fakeStatusService{err: service.ErrNotInstalled}); err != nil {
		t.Errorf("a fresh install was refused: %v", err)
	}
	// A status that could not be read (no permission, a platform quirk) is not
	// evidence of an installed service: install goes ahead and says what it says.
	if err := refuseReinstall(fakeStatusService{err: errors.New("permission denied")}); err != nil {
		t.Errorf("an unreadable status was taken for an installed service: %v", err)
	}
}

// The service manager's own refusal is explained the same way, for the case the
// status check could not see, and keeps the manager's words beside it.
func TestTheServiceManagersOwnRefusalIsExplained(t *testing.T) {
	cause := errors.New("Init already exists: /Library/LaunchDaemons/pingularity.plist")
	err := explainInstallErr(cause)
	for _, want := range []string{"already installed", elevate("pingularity restart"), cause.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the explained refusal does not say %q:\n%s", want, err)
		}
	}
	other := errors.New("permission denied")
	if got := explainInstallErr(other); got != other {
		t.Errorf("an unrelated install error was rewritten: %v", got)
	}
}

// controlCmd is where both are wired: the check before the service manager is
// asked to install, and the explanation of what it answers.
func TestControlCmdChecksForAnInstalledServiceBeforeInstalling(t *testing.T) {
	src := mustReadRepoFile(t, "main.go")
	start := strings.Index(src, "func controlCmd(")
	if start < 0 {
		t.Fatal("main.go has no controlCmd")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	check, ask := strings.Index(body, "refuseReinstall(s)"), strings.Index(body, "service.Control(s, action)")
	if check < 0 || ask < 0 || check > ask {
		t.Errorf("controlCmd does not check for an installed service before asking the service manager to install (check at %d, control at %d)", check, ask)
	}
	if !strings.Contains(body, "explainInstallErr(err)") {
		t.Error("controlCmd passes the service manager's install refusal through unexplained")
	}
}
