package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
)

// A SETTING THE OPERATOR TYPED AND CAN RETYPE MUST COME BACK AS SUCH.
//
// settings.Update refuses an iperf3 password beginning with the reserved seal
// prefix, and the sentence it returns names the remedy ("choose a different
// password"). Every Update error used to go through internalError, which logs
// server-side and answers a generic 500 whose body - "internal server error" -
// is byte-identical to the one the panic handler writes. The drawer put that in
// its footer, reddened Save, and left the operator looking at what reads as a
// daemon crash with nothing pointing at the password field. Worse, it stuck:
// while that row held the offending password, EVERY save from every tab failed
// the same opaque way.
func TestSettingsValidationErrorIsA400WithItsOwnMessage(t *testing.T) {
	s := newTestServer(t)
	body := `{"iperf_servers":[{"label":"t","addr":"192.0.2.1:5201","auth":true,"username":"u","rsa_key":"k","password":"enc:v1:abc"}]}`
	rr := do(t, s.Handler(), "POST", "/api/settings", body)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (a value the operator can fix is not a daemon failure)", rr.Code)
	}
	got := rr.Body.String()
	if strings.Contains(got, "internal server error") {
		t.Fatalf("body = %q; that is the panic handler's wording, so it reads as a crash", got)
	}
	for _, want := range []string{"enc:v1:", "choose a different password"} {
		if !strings.Contains(got, want) {
			t.Fatalf("body = %q, want it to contain %q - the remedy is the point", got, want)
		}
	}
	// The same field with an ordinary password still saves, so the gate is the
	// prefix and not the presence of a password.
	if rr := do(t, s.Handler(), "POST", "/api/settings",
		`{"iperf_servers":[{"label":"t","addr":"192.0.2.1:5201","auth":true,"username":"u","rsa_key":"k","password":"goodpass"}]}`); rr.Code != 200 {
		t.Fatalf("ordinary password: status = %d body %q, want 200", rr.Code, rr.Body.String())
	}
}

// The handler tells the two classes of failure apart by the sentinel, so pin
// that the controller really marks this one - a plain fmt.Errorf here would put
// the 500 back without changing a line of web.go.
func TestUpdateMarksAnOperatorFixableError(t *testing.T) {
	s := newTestServer(t)
	_, err := s.settings.Update(context.Background(), settings.Patch{
		IperfServers: []settings.IperfTarget{{Label: "t", Addr: "192.0.2.1:5201", Password: "enc:v1:abc"}},
	})
	if err == nil {
		t.Fatal("Update accepted a password beginning with the reserved seal prefix")
	}
	if !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("err = %v; it does not match settings.ErrInvalid, so the API cannot tell it from a store failure", err)
	}
	if strings.HasPrefix(err.Error(), settings.ErrInvalid.Error()) {
		t.Fatalf("err = %q; the sentinel's own words are in front of the operator's sentence", err.Error())
	}
}
