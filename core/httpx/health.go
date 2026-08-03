package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Health returns handlers for /healthz and /readyz. Convention:
//
//	/healthz — process is alive; does not touch the DB. Cheap. K8s liveness.
//	/readyz  — DB reachable AND migrations at the expected user_version.
//	           This is what a load balancer should gate on.
func Health(d *db.DB, expectedVersion int) (livez, readyz http.Handler) {
	livez = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	readyz = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		var version int
		if err := d.Read.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"status": "db_unreachable", "err": err.Error()})
			return
		}
		if version != expectedVersion {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":           "schema_stale",
				"user_version":     version,
				"expected_version": expectedVersion,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"user_version": version,
		})
	})
	return
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
