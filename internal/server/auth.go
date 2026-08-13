package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Glance-Studios/Lodestone/internal/target"
)

// targetHandler is a handler that has already been resolved to a target.
type targetHandler func(w http.ResponseWriter, r *http.Request, t *targetState)

// requireTargetToken resolves {target} from the path and checks the bearer token
// against that target's own token.
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

		if !tokenValid(t.Config.Token, bearerToken(r)) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r, t)
	})
}

// tokenValid reports whether got equals expected, compared in constant time so
// the check leaks no timing information about the token.
// An empty expected token is never valid.
func tokenValid(expected, got string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
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
