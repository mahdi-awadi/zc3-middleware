package api

// Gym membership endpoints. All state lives in the gym store; the panel is a
// best-effort mirror: linking a card enrolls it on the C3 with NoPanelAuth
// (the panel denies by default and the gym engine is the sole gate), unlinking
// revokes it. If the panel is offline the store still updates and the response
// carries "panel_synced": false.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/gym"
)

// SetGym attaches the gym store + gate engine.
func (s *Server) SetGym(st *gym.Store, e *gym.Engine) { s.gym, s.gymEngine = st, e }

// SetGateSwitch registers the function that repoints the live panel
// connection at a new address (wired from main: SetAddr + Close, after which
// the supervisor redials).
func (s *Server) SetGateSwitch(fn func(addr string)) { s.gateSwitch = fn }

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
	mux.HandleFunc("GET /gym/members/{id}/photo", s.gymGuard(s.handleGymPhotoGet))
	mux.HandleFunc("PUT /gym/members/{id}/photo", s.gymGuard(s.handleGymPhotoSet))
	mux.HandleFunc("DELETE /gym/members/{id}/photo", s.gymGuard(s.handleGymPhotoDelete))

	mux.HandleFunc("GET /gym/cards", s.gymGuard(s.handleGymCards))
	mux.HandleFunc("POST /gym/cards", s.gymGuard(s.handleGymCardRegister))
	mux.HandleFunc("PUT /gym/cards/{serial}", s.gymGuard(s.handleGymCardStatus))

	mux.HandleFunc("GET /gym/gates", s.gymGuard(s.handleGymGates))
	mux.HandleFunc("POST /gym/gates", s.gymGuard(s.handleGymGateAdd))
	mux.HandleFunc("POST /gym/gates/scan", s.gymGuard(s.handleGymGateScan))
	mux.HandleFunc("PUT /gym/gates/plans", s.gymGuard(s.handleGymPlanGates))
	mux.HandleFunc("PUT /gym/gates/{id}", s.gymGuard(s.handleGymGateUpdate))
	mux.HandleFunc("DELETE /gym/gates/{id}", s.gymGuard(s.handleGymGateDelete))
	mux.HandleFunc("POST /gym/gates/{id}/connect", s.gymGuard(s.handleGymGateConnect))
	mux.HandleFunc("POST /gym/gates/{id}/open", s.gymGuard(s.handleGymGateOpen))

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

// ---- member photos ----

// handleGymPhotoSet stores a member photo. Body: {"data":"data:image/jpeg;base64,..."}
// (a bare base64 string is also accepted).
func (s *Server) handleGymPhotoSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data string `json:"data"`
	}
	if !decode(w, r, &body) {
		return
	}
	b64 := body.Data
	if i := strings.Index(b64, ";base64,"); i >= 0 {
		b64 = b64[i+len(";base64,"):]
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid base64 image data"})
		return
	}
	if ct := http.DetectContentType(raw); !strings.HasPrefix(ct, "image/") {
		writeJSON(w, 400, map[string]string{"error": "not an image: " + ct})
		return
	}
	if err := s.gym.SavePhoto(r.PathValue("id"), raw); err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleGymPhotoGet(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gym.Photo(r.PathValue("id"))
	if err != nil {
		gymErr(w, err)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(raw))
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(raw)
}

func (s *Server) handleGymPhotoDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.gym.DeletePhoto(r.PathValue("id")); err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
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

// ---- gates (physical entrances) ----

// handleGymGates lists gates, the active one, and the plan→gate map.
func (s *Server) handleGymGates(w http.ResponseWriter, r *http.Request) {
	cfg := s.gym.Config()
	ready := s.ready == nil || s.ready()
	writeJSON(w, 200, map[string]any{
		"gates":      s.gym.Gates(),
		"active":     cfg.ActiveGate,
		"plan_gates": cfg.PlanGates,
		"connected":  ready,
	})
}

// handleGymGateScan sweeps the local network for ZKAccess C3 panels.
func (s *Server) handleGymGateScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var skip []string
	if ag := s.gym.ActiveGate(); ag != nil {
		skip = append(skip, ag.Addr)
	}
	devices := s.scan(ctx, skip...)
	// Mark devices already registered as gates.
	known := map[string][]string{}
	for _, g := range s.gym.Gates() {
		known[g.Addr] = append(known[g.Addr], g.ID)
	}
	type row struct {
		gym.Device
		GateIDs []string `json:"gate_ids,omitempty"`
	}
	out := make([]row, 0, len(devices))
	for _, d := range devices {
		out = append(out, row{Device: d, GateIDs: known[d.Addr]})
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

func (s *Server) handleGymGateAdd(w http.ResponseWriter, r *http.Request) {
	var g gym.Gate
	if !decode(w, r, &g) {
		return
	}
	added, err := s.gym.AddGate(g, time.Now())
	if err != nil {
		gymErr(w, err)
		return
	}
	// If this became the active gate (first gate), point the connection at it.
	if cfg := s.gym.Config(); cfg.ActiveGate == added.ID && s.gateSwitch != nil {
		s.gateSwitch(added.Addr)
	}
	writeJSON(w, 201, added)
}

func (s *Server) handleGymGateUpdate(w http.ResponseWriter, r *http.Request) {
	var patch gym.Gate
	if !decode(w, r, &patch) {
		return
	}
	g, err := s.gym.UpdateGate(r.PathValue("id"), patch)
	if err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) handleGymGateDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.gym.DeleteGate(r.PathValue("id")); err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// handleGymGateConnect makes a gate's panel the live connection.
func (s *Server) handleGymGateConnect(w http.ResponseWriter, r *http.Request) {
	g, err := s.gym.SetActiveGate(r.PathValue("id"))
	if err != nil {
		gymErr(w, err)
		return
	}
	if s.gateSwitch != nil {
		s.gateSwitch(g.Addr)
	}
	writeJSON(w, 200, map[string]any{"status": "connecting", "gate": g})
}

// handleGymGateOpen pulses a gate's door relay (reception manual open).
func (s *Server) handleGymGateOpen(w http.ResponseWriter, r *http.Request) {
	g, err := s.gym.Gate(r.PathValue("id"))
	if err != nil {
		gymErr(w, err)
		return
	}
	if ag := s.gym.ActiveGate(); ag == nil || ag.Addr != g.Addr {
		writeJSON(w, 409, map[string]string{"error": "البوابة ليست على اللوحة المتصلة — اتصل بها أولاً"})
		return
	}
	var body struct {
		Seconds int `json:"seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Seconds <= 0 {
		body.Seconds = s.opt.DoorDefaultSecs
	}
	err = s.panel.OpenDoor(r.Context(), g.Door, body.Seconds)
	respond(w, map[string]any{"status": "ok", "gate": g.ID, "door": g.Door, "seconds": body.Seconds}, err)
}

// handleGymPlanGates replaces the access-level → gates mapping.
func (s *Server) handleGymPlanGates(w http.ResponseWriter, r *http.Request) {
	var m map[string][]string
	if !decode(w, r, &m) {
		return
	}
	if err := s.gym.SetPlanGates(m); err != nil {
		gymErr(w, err)
		return
	}
	writeJSON(w, 200, s.gym.Config().PlanGates)
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
// {"card_no":"...", "door":1, "dry_run":true} — dry_run skips the door relay.
func (s *Server) handleGymSimulate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CardNo string `json:"card_no"`
		Door   int    `json:"door"`
		DryRun bool   `json:"dry_run"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.CardNo == "" {
		writeJSON(w, 400, map[string]string{"error": "card_no is required"})
		return
	}
	u := s.gymEngine.Simulate(body.CardNo, body.Door, !body.DryRun)
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
