package httpapi

import (
	"net/http"

	"github.com/mrchypark/beesuh"
)

func (h handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	workspace, requestID := query.Get("workspace_id"), r.PathValue("request_id")
	if len(query) != 1 || len(query["workspace_id"]) != 1 || !validID(workspace) || !validID(requestID) || (r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0) {
		h.fail(w, beesuh.ErrInvalidInput, beesuh.Result{State: "failed"})
		return
	}
	out, err := h.runtime.CancelRun(r.Context(), workspace, requestID)
	if err != nil {
		h.fail(w, err, beesuh.Result{State: "failed"})
		return
	}
	status := http.StatusOK
	if out.State == "running" {
		status = http.StatusAccepted
	}
	// Cancel permission does not grant read access to a completed run's result.
	h.writeJSON(w, status, struct {
		RequestID             string `json:"request_id"`
		State                 string `json:"state"`
		CancellationRequested bool   `json:"cancellation_requested"`
	}{requestID, out.State, out.CancellationRequested})
}
