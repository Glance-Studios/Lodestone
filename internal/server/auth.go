package server

import (
	"net/http"
	"strings"

	"github.com/Glance-Studios/Lodestone/internal/target"
)

// targetHandler is a handler that has already been resolved to a target and to
// the credential that authenticated the request.
//
// The identity is passed as an argument rather than stashed in the request
// context: a handler that records who deployed must not be able to compile
// without it.
type targetHandler func(w http.ResponseWriter, r *http.Request, t *targetState, by string)

// requireTargetToken resolves {target} from the path and checks the bearer token
// against that target's own credentials.
//
// Auth cannot come before resolution, because which token is correct depends on
// which target is addressed - that is what makes a dev credential unable to
// reach prod.
//
// An unknown target answers 404 rather than 401. That lets a caller with a valid
// token learn they typed the name wrong, at the cost of letting an unauthorised
// caller discover which names exist. Target names are not secrets - they are
// dev-lobby and prod-lobby - and a name alone deploys nothing.
func (s *Server) requireTargetToken(next targetHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("target")

		// Validate the shape before looking it up, so a hostile path segment is
		// rejected by the same rule that constrains the config.
		if !target.ValidName(name) {
			http.Error(w, "invalid target name", http.StatusBadRequest)
			return
		}

		t, ok := s.targets[name]
		if !ok {
			http.Error(w, "no such target", http.StatusNotFound)
			return
		}

		by, ok := t.Config.Authenticate(bearerToken(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r, t, by)
	})
}

// bearerToken returns the token from an "Authorization: Bearer <token>" header,
// or "" if the header is absent or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}
