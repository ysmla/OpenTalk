package health

import (
	"context"
	"net/http"
	"opentalk/server/internal/platform/httpapi"
	"time"
)

type Probe func(context.Context) error

func Register(mux *http.ServeMux, mysql, redis Probe) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { httpapi.JSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if mysql(ctx) != nil || redis(ctx) != nil {
			httpapi.Fail(w, r, 503, "NOT_READY", "服务依赖尚未就绪")
			return
		}
		httpapi.JSON(w, 200, map[string]string{"status": "ok"})
	})
}
