package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/inkeliz/poudrine/internal/database"
)

// invalidQuery identifies a well-formed JSON object with invalid field values.
type invalidQuery string

func (e invalidQuery) Error() string { return string(e) }

func queryJSON[A, R any](read func(context.Context, A) (R, error), writeError func(http.ResponseWriter, error)) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		var args A
		if !queryArgs(w, body, &args) {
			return
		}
		view, err := read(r.Context(), args)
		if err != nil {
			if bad, ok := errors.AsType[invalidQuery](err); ok {
				http.Error(w, bad.Error(), http.StatusBadRequest)
			} else {
				writeError(w, err)
			}
			return
		}
		adminJSON(w, view)
	}
}

func writeDBQueryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		http.Error(w, "404 page not found", http.StatusNotFound)
	case errors.Is(err, database.ErrBuildInterval), errors.Is(err, database.ErrBuildPeriod):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		adminInternal(w, err)
	}
}
