package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadiness(t *testing.T) {
	for _, broken := range []string{"", "mysql", "redis"} {
		t.Run(broken, func(t *testing.T) {
			probe := func(name string) Probe {
				return func(context.Context) error {
					if name == broken {
						return errors.New("secret connection details")
					}
					return nil
				}
			}
			mux := http.NewServeMux()
			Register(mux, probe("mysql"), probe("redis"))
			for _, path := range []string{"/health", "/ready"} {
				res := httptest.NewRecorder()
				mux.ServeHTTP(res, httptest.NewRequest("GET", path, nil))
				want := 200
				if path == "/ready" && broken != "" {
					want = 503
				}
				if res.Code != want {
					t.Errorf("%s status=%d want=%d", path, res.Code, want)
				}
				if strings.Contains(res.Body.String(), "secret") {
					t.Fatal("leaks dependency details")
				}
			}
		})
	}
}
