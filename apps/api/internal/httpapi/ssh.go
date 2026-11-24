package httpapi

import (
	"net/http"

	"github.com/coachpo/cloudex/apps/api/internal/remote"
)

func (s *Server) access(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := shortCall(r)
	defer cancel()
	view, e := s.Control.Access(ctx, workspaceID(r))
	send(w, r, view, e)
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PublicKeys *[]string `json:"public_keys"`
	}
	if !parse(w, r, &in, false) {
		return
	}
	if in.PublicKeys == nil {
		bad(w, r, "Send public_keys, or an empty array to disable SSH.")
		return
	}
	keys, e := remote.Keys(*in.PublicKeys)
	if e != nil {
		bad(w, r, e.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 {
		bad(w, r, "Idempotency-Key is too long.")
		return
	}
	id := workspaceID(r)
	o, e := s.Store.SetSSH(r.Context(), id, keys, key, fingerprint([]any{"set-ssh", id, keys}))
	if e != nil {
		failure(w, r, e)
		return
	}
	accepted(w, r, o)
}
