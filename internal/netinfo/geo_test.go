package netinfo

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// geoResp builds a fresh canned response (new body each call, so a host can be hit
// more than once without a consumed-body error).
func geoResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

// geoStub routes requests by host so the two geo providers can be driven
// independently. An empty body for a host => that provider returns HTTP 500.
func geoStub(bodies map[string]string) *http.Client {
	return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		for host, body := range bodies {
			if strings.Contains(r.URL.Host, host) {
				if body == "" {
					return geoResp(500, ""), nil
				}
				return geoResp(200, body), nil
			}
		}
		return geoResp(404, ""), nil
	})}
}

// publicIPGeo geolocates the host's own IP via ipwho.is, falling back to geojs.io
// when the primary errors, rate-limits (success:false), or returns no city.
func TestPublicIPGeo(t *testing.T) {
	const okWho = `{"success":true,"city":"Toronto","country_code":"CA"}`
	const failWho = `{"success":false,"message":"reserved range"}`
	const emptyWho = `{"success":true,"city":"","country_code":""}`
	const okGeo = `{"city":"Ottawa","country_code":"CA"}`

	cases := []struct {
		name             string
		who, geo         string // "" => that provider returns HTTP 500
		wantCity, wantCC string
		wantOK           bool
	}{
		{"primary hit", okWho, okGeo, "Toronto", "CA", true},
		{"primary error -> fallback", "", okGeo, "Ottawa", "CA", true},
		{"primary rate-limited -> fallback", failWho, okGeo, "Ottawa", "CA", true},
		{"primary empty city -> fallback", emptyWho, okGeo, "Ottawa", "CA", true},
		{"both fail", "", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.http = geoStub(map[string]string{"ipwho.is": c.who, "geojs.io": c.geo})
			city, cc, ok := publicIPGeo(context.Background(), m, "1.2.3.4")
			if ok != c.wantOK || city != c.wantCity || cc != c.wantCC {
				t.Errorf("got (%q,%q,%v), want (%q,%q,%v)", city, cc, ok, c.wantCity, c.wantCC, c.wantOK)
			}
		})
	}
}
