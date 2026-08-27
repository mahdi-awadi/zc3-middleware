package api

// Gym membership endpoints. All state lives in the gym store; the panel is a
// best-effort mirror: linking a card enrolls it on the C3 with NoPanelAuth
// (the panel denies by default and the gym engine is the sole gate), unlinking
// revokes it. If the panel is offline the store still updates and the response
// carries "panel_synced": false.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/gym"
)

// SetGym attaches the gym store + gate engine.
func (s *Server) SetGym(st *gym.Store, e *gym.Engine) { s.gym, s.gymEngine = st, e }

func (s *Server) gymRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /gym/summary", s.gymGuard(s.handleGymSummary))
	mux.HandleFunc("GET /gym/config", s.gymGuard(s.handleGymConfig))
	mux.HandleFunc("PUT /gym/config", s.gymGuard(s.handleGymConfigSet))

	mux.HandleFunc("GET /gym/members", s.gymGuard(s.handleGymMembers))
	mux.HandleFunc("POST /gym/members", s.gymGuard(s.handleGymMemberAdd))
	mux.HandleFunc("GET /gym/members/{id}", s.gymGuard(s.handleGymMember))
	mux.HandleFunc("PUT /gym/members/{id}", s.gymGuard(s.handleGymMemberUpdate))
	mux.HandleFunc("DELETE /gym/members/{id}", s.gymGuard(s.handleGymMemberDelete))
	mux.HandleFunc("POST /gym/members/{id}/renew", s.gymGuard(s.handleGymRenew))
	mux.HandleFunc("POST /gym/members/{id}/freeze", s.gymGuard(s.handleGymFreeze))
	mux.HandleFunc("POST /gym/members/{id}/unfreeze", s.gymGuard(s.handleGymUnfreeze))
	mux.HandleFunc("POST /gym/members/{id}/card", s.gymGuard(s.handleGymLinkCard))
	mux.HandleFunc("DELETE /gym/members/{id}/card", s.gymGuard(s.handleGymUnlinkCard))
	mux.HandleFunc("GET /gym/members/{id}/attendance", s.gymGuard(s.handleGymAttendance))
	mux.HandleFunc("GET /gym/members/{id}/checkins", s.gymGuard(s.handleGymMemberCheckins))

	mux.HandleFunc("GET /gym/cards", s.gymGuard(s.handleGymCards))
	mux.HandleFunc("POST /gym/cards", s.gymGuard(s.handleGymCardRegister))
	mux.HandleFunc("PUT /gym/cards/{serial}", s.gymGuard(s.handleGymCardStatus))

	mux.HandleFunc("GET /gym/checkins", s.gymGuard(s.handleGymCheckins))
	mux.HandleFunc("GET /gym/scan", s.gymGuard(s.handleGymScan))
	mux.HandleFunc("DELETE /gym/scan", s.gymGuard(s.handleGymScanClear))
	mux.HandleFunc("POST /gym/simulate", s.gymGuard(s.handleGymSimulate))
	mux.HandleFunc("GET /gym/gate", s.gymGuard(s.handleGymGate))
}

// gymGuard 503s every gym route when the subsystem is not wired.
func (s *Server) gymGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.gym == nil || s.gymEngine == nil {
			writeJSON(w, 503, map[string]string{"error": "gym subsystem unavailable"})
			return
		}
		h(w, r)
	}
}

func gymErr(w http.ResponseWriter, err error) {
	code := 400
	if errors.Is(err, gym.ErrNotFound) {
		code = 404
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) handleGymSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.gym.Summarize(time.Now()))
}

func (s *Server) handleGymConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.gym.Config())
}

func (s *Server) handleGymConfigSet(w http.ResponseWriter, r *http.Request) {
	var c gym.Config
	if !decode(w, r, &c) {
		return
	}
	if err := s.gym.SetConfig(c); err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, s.gym.Config())
}

// ---- members ----

func (s *Server) handleGymMembers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, 200, s.gym.Members(q.Get("status"), q.Get("q"), time.Now()))
}

// memberBody is the create-member payload: personal data + initial plan.
type memberBody struct {
	gym.Member
	Plan gym.NewSubInput `json:"plan"`
}

func (s *Server) handleGymMemberAdd(w http.ResponseWriter, r *http.Request) {
	var body memberBody
	if !decode(w, r, &body) {
		return
	}
	v, err := s.gym.AddMember(body.Member, body.Plan, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	synced := true
	if v.CardNo != "" {
		synced = s.panelEnroll(r, v.CardNo, v.Name)
	}
	writeJSON(w, 201, map[string]any{"member": v, "panel_synced": synced})
}

func (s *Server) handleGymMember(w http.ResponseWriter, r *http.Request) {
	v, err := s.gym.Member(r.PathValue("id"), time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleGymMemberUpdate(w http.ResponseWriter, r *http.Request) {
	var patch gym.Member
	if !decode(w, r, &patch) {
		return
	}
	v, err := s.gym.UpdateMember(r.PathValue("id"), patch, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleGymMemberDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, err := s.gym.Member(id, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	if err := s.gym.DeleteMember(id); err != nil {
		gymErr(w, err)
		return
	}
	synced := true
	if v.CardNo != "" {
		synced = s.panelRevoke(r, v.CardNo)
	}
	writeJSON(w, 200, map[string]any{"status": "deleted", "panel_synced": synced})
}

func (s *Server) handleGymRenew(w http.ResponseWriter, r *http.Request) {
	var in gym.NewSubInput
	if !decode(w, r, &in) {
		return
	}
	v, err := s.gym.Renew(r.PathValue("id"), in, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleGymFreeze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Days   int    `json:"days"`
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	v, err := s.gym.Freeze(r.PathValue("id"), body.Days, body.Reason, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleGymUnfreeze(w http.ResponseWriter, r *http.Request) {
	v, err := s.gym.Unfreeze(r.PathValue("id"), time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// ---- card link / unlink ----

// panelEnroll mirrors a linked card onto the C3 as a NoPanelAuth user so the
// panel itself never opens the door — the gym engine is the sole gate.
// Returns false when the panel could not be updated (offline / writes off).
func (s *Server) panelEnroll(r *http.Request, cardNo, name string) bool {
	err := s.panel.SetCard(r.Context(), c3.Card{
		PIN: cardNo, CardNo: cardNo, Name: name,
		Doors: []int{1, 2, 3, 4}, NoPanelAuth: true,
	})
	if err != nil {
		s.log.Warn("gym: panel enroll failed", "card", cardNo, "err", err)
		return false
	}
	return true
}

func (s *Server) panelRevoke(r *http.Request, cardNo string) bool {
	if err := s.panel.DeleteCard(r.Context(), cardNo); err != nil {
		s.log.Warn("gym: panel revoke failed", "card", cardNo, "err", err)
		return false
	}
	return true
}

func (s *Server) handleGymLinkCard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardNo string `json:"card_no"`
	}
	if !decode(w, r, &body) {
		return
	}
	v, err := s.gym.LinkCard(r.PathValue("id"), body.CardNo, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	s.gymEngine.ClearScan()
	writeJSON(w, 200, map[string]any{"member": v, "panel_synced": s.panelEnroll(r, v.CardNo, v.Name)})
}

func (s *Server) handleGymUnlinkCard(w http.ResponseWriter, r *http.Request) {
	v, serial, err := s.gym.UnlinkCard(r.PathValue("id"), time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"member": v, "panel_synced": s.panelRevoke(r, serial)})
}

// ---- cards registry ----

func (s *Server) handleGymCards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.gym.Cards())
}

func (s *Server) handleGymCardRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Serial string `json:"serial"`
	}
	if !decode(w, r, &body) {
		return
	}
	c, err := s.gym.RegisterCard(body.Serial, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) handleGymCardStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &body) {
		return
	}
	c, err := s.gym.SetCardStatus(r.PathValue("serial"), body.Status)
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, c)
}

// ---- gate / attendance ----

func (s *Server) handleGymCheckins(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	writeJSON(w, 200, s.gym.Checkins("", limit))
}

func (s *Server) handleGymMemberCheckins(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	writeJSON(w, 200, s.gym.Checkins(r.PathValue("id"), limit))
}

func (s *Server) handleGymAttendance(w http.ResponseWriter, r *http.Request) {
	weeks := atoiDefault(r.URL.Query().Get("weeks"), 16)
	if weeks < 1 || weeks > 52 {
		weeks = 16
	}
	writeJSON(w, 200, s.gym.Attendance(r.PathValue("id"), weeks*7, time.Now()))
}

// handleGymScan reports the last swipe seen at any reader (card-link flow).
func (s *Server) handleGymScan(w http.ResponseWriter, r *http.Request) {
	sc := s.gymEngine.LastScan()
	if sc == nil {
		writeJSON(w, 200, map[string]any{"waiting": true})
		return
	}
	writeJSON(w, 200, map[string]any{"waiting": false, "scan": sc})
}

func (s *Server) handleGymScanClear(w http.ResponseWriter, r *http.Request) {
	s.gymEngine.ClearScan()
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleGymSimulate injects a swipe (demo / desk enrollment without hardware).
// {"card_no":"...", "dry_run":true} — dry_run skips the door relay.
func (s *Server) handleGymSimulate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardNo string `json:"card_no"`
		DryRun bool   `json:"dry_run"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.CardNo == "" {
		writeJSON(w, 400, map[string]string{"error": "card_no is required"})
		return
	}
	u := s.gymEngine.Simulate(body.CardNo, !body.DryRun)
	writeJSON(w, 200, u)
}

// handleGymGate streams gate decisions as SSE for the kiosk screen.
func (s *Server) handleGymGate(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	ch := s.gymEngine.Subscribe(s.opt.EventBufferSize)
	defer s.gymEngine.Unsubscribe(ch)
	s.sseActive.Add(1)
	defer s.sseActive.Add(-1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case u := <-ch:
			b, _ := json.Marshal(u)
			w.Write([]byte("event: gate\ndata: "))
			w.Write(b)
			w.Write([]byte("\n\n"))
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}
