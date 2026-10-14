package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := map[string]struct {
		trustProxy bool
		remoteAddr string
		forwarded  string
		want       string
	}{
		"direct peer":              {false, "203.0.113.7:44321", "", "203.0.113.7"},
		"forwarded ignored":        {false, "203.0.113.7:44321", "198.51.100.9", "203.0.113.7"},
		"trusted first entry":      {true, "10.0.0.1:44321", "198.51.100.9", "198.51.100.9"},
		"trusted list takes first": {true, "10.0.0.1:44321", "198.51.100.9, 10.0.0.5", "198.51.100.9"},
		"trusted with spaces":      {true, "10.0.0.1:44321", "  198.51.100.9 , 10.0.0.5", "198.51.100.9"},
		"trusted but empty header": {true, "10.0.0.1:44321", "", "10.0.0.1"},
		"trusted but blank entry":  {true, "10.0.0.1:44321", "   ", "10.0.0.1"},
		"peer without port":        {false, "203.0.113.7", "", "203.0.113.7"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/api/v1/auth/email-codes", nil)
			request.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				request.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			deps := Deps{TrustProxy: tc.trustProxy}
			if got := deps.clientIP(request); got != tc.want {
				t.Fatalf("clientIP=%q want=%q", got, tc.want)
			}
		})
	}
}

// The header must never be honoured by default: a client that reaches the API directly can set it.
func TestClientIPIgnoresForgedHeaderByDefault(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/email-codes", nil)
	request.RemoteAddr = "203.0.113.7:1234"
	request.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := (Deps{}).clientIP(request); got != "203.0.113.7" {
		t.Fatalf("clientIP=%q, a forged header was trusted", got)
	}
}
