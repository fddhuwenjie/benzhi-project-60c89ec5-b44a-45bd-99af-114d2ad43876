package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"restoration-quality/internal/application"
	"restoration-quality/internal/domain"
	"restoration-quality/internal/persistence"
	"sort"
	"strconv"
	"strings"
	"time"
)

func allHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
func validEvidenceMetadata(kind string, m map[string]string) bool {
	if m == nil {
		return false
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return false
		}
	}
	k := strings.ToLower(strings.TrimSpace(kind))
	if k != "photo" && k != "document" && k != "measurement" && k != "instrument" && k != "report" {
		return false
	}
	if k == "photo" {
		return strings.TrimSpace(m["camera"]) != "" || strings.TrimSpace(m["description"]) != "" || strings.TrimSpace(m["拍摄说明"]) != ""
	}
	if k == "instrument" || k == "measurement" {
		return strings.TrimSpace(m["device"]) != "" || strings.TrimSpace(m["instrument"]) != "" || strings.TrimSpace(m["设备"]) != ""
	}
	return true
}
func archiveValid(p *domain.RestorationProject, a *domain.ReleaseArchive) bool {
	ids := make([]string, 0, len(p.Evidence))
	for _, e := range p.Evidence {
		if e.Superseded {
			continue
		}
		ids = append(ids, e.ID+e.SHA256)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "|")))
	root := fmt.Sprintf("%x", sum)
	if a.EvidenceRoot != root {
		return false
	}
	c := sha256.Sum256([]byte(a.ID + a.EvidenceRoot + a.ArchiveVersion))
	return a.Checksum == fmt.Sprintf("%x", c)
}

type Handler struct{ app *application.Service }

func New(app *application.Service) *Handler { return &Handler{app: app} }
func (h *Handler) Routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", h.health)
	m.HandleFunc("/v1/projects", h.projects)
	m.HandleFunc("/v1/projects/", h.projectSub)
	return m
}
func write(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, e error) {
	status := http.StatusBadRequest
	code := "invalid_request"
	if strings.HasPrefix(e.Error(), "environment_abnormal") {
		status = http.StatusConflict
		code = "environment_abnormal"
	}
	if strings.HasPrefix(e.Error(), "environment_trend_blocked") {
		status = http.StatusConflict
		code = "environment_trend_blocked"
	}
	if strings.HasPrefix(e.Error(), "due_at_out_of_window") {
		status = http.StatusUnprocessableEntity
		code = "due_at_out_of_window"
	}
	if strings.HasPrefix(e.Error(), "environment_snapshot_required") {
		status = http.StatusConflict
		code = "environment_snapshot_required"
	}
	if strings.HasPrefix(e.Error(), "duplicate_evidence") {
		status = http.StatusConflict
		code = "duplicate_evidence"
	}
	if strings.HasPrefix(e.Error(), "stale_snapshot") {
		status = http.StatusConflict
		code = "stale_snapshot"
	}
	if errors.Is(e, domain.ErrNotFound) {
		status = http.StatusNotFound
		code = "not_found"
	} else if errors.Is(e, domain.ErrConflict) {
		status = http.StatusConflict
		code = "conflict"
	} else if errors.Is(e, domain.ErrForbidden) {
		status = http.StatusUnprocessableEntity
		code = "forbidden"
	}
	body := map[string]interface{}{"error": code, "message": e.Error()}
	var fe *domain.FieldValidationError
	if errors.As(e, &fe) {
		body["error"] = "field_validation"
		body["fields"] = fe.Fields
	}
	var ce *domain.CapacityError
	if errors.As(e, &ce) {
		body["error"] = "capacity_exceeded"
		body["technician"] = ce.Technician
		body["workload_limit"] = ce.Limit
		body["workload"] = ce.Count
	}
	var pc *domain.ProjectConflictError
	if errors.As(e, &pc) {
		body["project_id"] = pc.ProjectID
		body["conflict_reason"] = pc.Reason
		body["next"] = "/v1/projects/" + pc.ProjectID + "/baseline"
	}
	write(w, status, body)
}
func decode(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return domain.ErrInvalid
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func actor(r *http.Request) string {
	a := r.Header.Get("X-Actor")
	if a == "" {
		a = r.Header.Get("X-Operator")
	}
	if a == "" {
		a = "anonymous"
	}
	return a
}
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	write(w, 200, map[string]string{"status": "ok"})
}

type createReq struct {
	AssetCode        string `json:"asset_code"`
	Title            string `json:"title"`
	Custodian        string `json:"custodian"`
	RequestID        string `json:"request_id"`
	Preflight        bool   `json:"preflight"`
	ReservationToken string `json:"reservation_token,omitempty"`
	Batch            bool   `json:"batch"`
	Projects         []struct {
		AssetCode string `json:"asset_code"`
		Title     string `json:"title"`
		Custodian string `json:"custodian"`
		RequestID string `json:"request_id"`
	} `json:"projects"`
}

func (h *Handler) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var q createReq
		if e := decode(r, &q); e != nil {
			fail(w, e)
			return
		}
		if q.Batch || len(q.Projects) > 0 {
			if !q.Batch {
				fail(w, domain.ErrInvalid)
				return
			}
			if strings.TrimSpace(actorHeader(r)) == "" {
				fail(w, domain.ErrInvalid)
				return
			}
			if q.Preflight || r.URL.Query().Get("preflight") == "true" {
				outs := make([]application.ProjectPreflight, len(q.Projects))
				for i, x := range q.Projects {
					if strings.TrimSpace(x.RequestID) == "" {
						fail(w, domain.ErrInvalid)
						return
					}
					pf, e := h.app.CreatePreflight(x.AssetCode, x.Title, x.Custodian, actor(r), x.RequestID)
					if e != nil {
						fail(w, e)
						return
					}
					outs[i] = pf
				}
				write(w, http.StatusOK, map[string]interface{}{"preflights": outs})
				return
			}
			items := make([]application.BatchProjectInput, len(q.Projects))
			for i, x := range q.Projects {
				items[i] = application.BatchProjectInput{AssetCode: x.AssetCode, Title: x.Title, Custodian: x.Custodian, RequestID: x.RequestID}
			}
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				key = q.RequestID
			}
			if key == "" {
				fail(w, domain.ErrInvalid)
				return
			}
			out, e := h.app.CreateProjectsBatch(items, actor(r), key)
			if e != nil {
				fail(w, e)
				return
			}
			write(w, http.StatusCreated, out)
			return
		}
		if strings.TrimSpace(q.RequestID) == "" || strings.TrimSpace(actorHeader(r)) == "" {
			fail(w, domain.ErrInvalid)
			return
		}
		if q.Preflight || r.URL.Query().Get("preflight") == "true" {
			out, e := h.app.CreatePreflight(q.AssetCode, q.Title, q.Custodian, actor(r), q.RequestID)
			if e != nil {
				fail(w, e)
				return
			}
			write(w, 200, out)
			return
		}
		token := q.ReservationToken
		if token == "" {
			token = r.Header.Get("X-Reservation-Token")
		}
		p, e := h.app.CreateProject(q.AssetCode, q.Title, q.Custodian, actor(r), q.RequestID, token)
		if e != nil {
			fail(w, e)
			return
		}
		b, _ := json.Marshal(p)
		var out map[string]interface{}
		_ = json.Unmarshal(b, &out)
		out["next"] = "/v1/projects/" + p.ID + "/baseline"
		write(w, 201, out)
		return
	}
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		if asset := strings.TrimSpace(q.Get("asset_code")); asset != "" {
			if strings.ContainsAny(asset, "\r\n") {
				fail(w, domain.ErrInvalid)
				return
			}
			p, e := h.app.GetByAsset(asset, actor(r), r.Header.Get("X-Request-ID"))
			if e != nil {
				fail(w, e)
				return
			}
			view := map[string]interface{}{"id": p.ID, "asset_code": p.AssetCode, "title": p.Title, "custodian": p.Custodian, "status": p.Status, "revision": p.PlanRevision}
			write(w, 200, map[string]interface{}{"projects": []map[string]interface{}{view}, "project": view, "total": 1, "match": "asset_code_exact"})
			return
		}
		status, risk, cust := q.Get("status"), q.Get("risk_level"), q.Get("custodian")
		if risk == "" {
			risk = q.Get("risk")
		}
		assetPrefix := q.Get("asset_prefix")
		validStatus := map[string]bool{"draft": true, "baselined": true, "in_progress": true, "inspection": true, "remediation": true, "released": true, "archived": true}
		if status != "" && !validStatus[status] || risk != "" && (risk != "low" && risk != "medium" && risk != "high") {
			fail(w, domain.ErrInvalid)
			return
		}
		page, size := 1, 20
		var e error
		if q.Get("page") != "" {
			page, e = strconv.Atoi(q.Get("page"))
		}
		if e != nil || page < 1 {
			fail(w, domain.ErrInvalid)
			return
		}
		if q.Get("page_size") != "" {
			size, e = strconv.Atoi(q.Get("page_size"))
		}
		if e != nil || size < 1 || size > 200 {
			fail(w, domain.ErrInvalid)
			return
		}
		parseTime := func(k string) (*time.Time, error) {
			v := q.Get(k)
			if v == "" {
				if k == "created_since" {
					v = q.Get("created_from")
				}
				if k == "created_until" {
					v = q.Get("created_to")
				}
			}
			if v == "" {
				return nil, nil
			}
			t, e := time.Parse(time.RFC3339, v)
			return &t, e
		}
		since, e := parseTime("created_since")
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		until, e := parseTime("created_until")
		if e != nil || since != nil && until != nil && until.Before(*since) {
			fail(w, domain.ErrInvalid)
			return
		}
		items, total, stats := h.app.ListProjectsFiltered(persistence.ProjectFilter{Status: status, RiskLevel: risk, Custodian: cust, AssetPrefix: assetPrefix, CreatedSince: since, CreatedUntil: until}, page, size)
		h.app.AuditQuery(actor(r), r.Header.Get("X-Request-ID"), "asset_prefix="+assetPrefix+" custodian="+cust+" status="+status+" risk="+risk+" created_since="+q.Get("created_since")+" created_until="+q.Get("created_until"))
		riskCounts := map[string]int{"low": 0, "medium": 0, "high": 0}
		allFiltered, _, _ := h.app.ListProjectsFiltered(persistence.ProjectFilter{Status: status, RiskLevel: risk, Custodian: cust, AssetPrefix: assetPrefix, CreatedSince: since, CreatedUntil: until}, 1, 1<<30)
		for _, p := range allFiltered {
			if _, ok := riskCounts[p.RiskLevel]; ok {
				riskCounts[p.RiskLevel]++
			}
		}
		write(w, 200, map[string]interface{}{"page": page, "page_size": size, "total": total, "projects": items, "status_counts": stats, "risk_counts": riskCounts, "risk_distribution": riskCounts})
		return
	}
	write(w, 405, nil)
}

func actorHeader(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Actor")); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Operator"))
}
func (h *Handler) projectSub(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/projects/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		write(w, 404, nil)
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		p, e := h.app.GetProject(id)
		if e != nil {
			fail(w, e)
			return
		}
		view, _ := h.app.ProcedureSummary(p, r.URL.Query().Get("technician"), r.URL.Query().Get("pending") == "true")
		b, _ := json.Marshal(p)
		var out map[string]interface{}
		_ = json.Unmarshal(b, &out)
		out["procedures_summary"] = view
		write(w, 200, out)
		return
	}
	if len(parts) == 2 && parts[1] == "baseline" && r.Method == http.MethodPut {
		h.baseline(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "baseline" && parts[2] == "history" && r.Method == http.MethodGet {
		rev, _ := strconv.Atoi(r.URL.Query().Get("revision"))
		hst, e := h.app.BaselineHistory(id, rev)
		if e != nil {
			fail(w, e)
			return
		}
		h.app.AuditQueryProject(id, actor(r), r.Header.Get("X-Request-ID"), "baseline_history")
		write(w, 200, map[string]interface{}{"history": hst})
		return
	}
	if len(parts) == 3 && parts[1] == "baseline" && parts[2] == "history" && r.Method == http.MethodPost {
		var q struct {
			Revision  int    `json:"revision"`
			RequestID string `json:"request_id"`
			Reason    string `json:"reason"`
		}
		if e := decode(r, &q); e != nil || q.Revision <= 0 {
			fail(w, domain.ErrInvalid)
			return
		}
		p, e := h.app.RollbackBaseline(id, q.Revision, expected(r), actor(r), q.RequestID, q.Reason)
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, p)
		return
	}
	if len(parts) >= 2 && parts[1] == "procedures" {
		if len(parts) == 3 && parts[2] == "batch" && r.Method == http.MethodPost {
			h.addProcedure(w, r, id)
			return
		}
		if len(parts) == 3 && parts[2] == "reorder" && r.Method == http.MethodPost {
			h.reorder(w, r, id)
			return
		}
		if len(parts) == 4 && (parts[3] == "pause" || parts[3] == "resume" || parts[3] == "start") && r.Method == http.MethodPost {
			h.pauseResume(w, r, id, parts[2], parts[3])
			return
		}
		if len(parts) == 2 && r.Method == http.MethodPost {
			h.addProcedure(w, r, id)
			return
		}
		if len(parts) == 4 && parts[3] == "complete" && r.Method == http.MethodPost {
			h.complete(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "reopen" && r.Method == http.MethodPost {
			var q struct {
				InspectionID string `json:"inspection_id"`
				Reason       string `json:"reason"`
				RequestID    string `json:"request_id"`
			}
			if e := decode(r, &q); e != nil {
				fail(w, e)
				return
			}
			if q.RequestID == "" {
				q.RequestID = r.Header.Get("Idempotency-Key")
			}
			p, e := h.app.ReopenProcedure(id, parts[2], q.InspectionID, q.Reason, actor(r), q.RequestID, expected(r))
			if e != nil {
				fail(w, e)
				return
			}
			write(w, 200, p)
			return
		}
	}
	if len(parts) == 3 && parts[1] == "evidence" && parts[2] == "coverage" && r.Method == http.MethodGet {
		v, e := h.app.EvidenceCoverage(id, r.URL.Query().Get("kind"))
		if e != nil {
			fail(w, e)
			return
		}
		if raw := r.URL.Query().Get("page_size"); raw != "" {
			size, er := strconv.Atoi(raw)
			page, ep := 1, error(nil)
			if r.URL.Query().Get("page") != "" {
				page, ep = strconv.Atoi(r.URL.Query().Get("page"))
			}
			if er != nil || ep != nil || size < 1 || size > 200 || page < 1 {
				fail(w, domain.ErrInvalid)
				return
			}
			if all, ok := v["evidence"].([]*domain.EvidenceItem); ok {
				start := (page - 1) * size
				if start > len(all) {
					start = len(all)
				}
				end := start + size
				if end > len(all) {
					end = len(all)
				}
				v["evidence"] = all[start:end]
				v["page"] = page
				v["page_size"] = size
				v["next_cursor"] = ""
				if end < len(all) && end > start {
					v["next_cursor"] = all[end-1].ID
				}
			}
		}
		write(w, 200, v)
		return
	}
	if len(parts) == 2 && parts[1] == "evidence" && r.Method == http.MethodGet {
		h.listEvidence(w, r, id)
		return
	}
	if len(parts) == 4 && parts[1] == "inspections" && parts[3] == "revisions" && r.Method == http.MethodPost {
		h.reviseInspection(w, r, id, parts[2])
		return
	}
	if len(parts) == 3 && parts[1] == "inspections" && r.Method == http.MethodPut {
		h.reviseInspection(w, r, id, parts[2])
		return
	}
	if len(parts) == 4 && parts[1] == "inspections" && parts[3] == "freeze" && r.Method == http.MethodPost {
		h.freezeInspection(w, r, id, parts[2])
		return
	}
	if len(parts) == 2 && parts[1] == "evidence-verification" && r.Method == http.MethodGet {
		if r.URL.Query().Get("coverage") == "true" {
			v, e := h.app.EvidenceCoverage(id, r.URL.Query().Get("kind"))
			if e != nil {
				fail(w, e)
				return
			}
			write(w, 200, v)
			return
		}
		v, e := h.app.VerifyEvidence(id, actor(r), r.Header.Get("X-Request-ID"))
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, v)
		return
	}
	if len(parts) == 3 && parts[1] == "evidence" && parts[2] == "verification" && r.Method == http.MethodGet {
		v, e := h.app.VerifyEvidence(id, actor(r), r.Header.Get("X-Request-ID"))
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, v)
		return
	}
	if len(parts) == 3 && parts[1] == "evidence" && parts[2] == "verify" && r.Method == http.MethodGet {
		v, e := h.app.VerifyEvidence(id, actor(r), r.Header.Get("X-Request-ID"))
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, v)
		return
	}
	if len(parts) == 3 && parts[1] == "remediations" && parts[2] == "reassign" && r.Method == http.MethodPost {
		h.reassignRemediations(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "remediations" && r.Method == http.MethodGet {
		h.listRemediations(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "remediations" && parts[2] == "resolve" && r.Method == http.MethodPost {
		h.resolveBatch(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "inspections" && r.Method == http.MethodPost {
		h.inspect(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "inspections" && r.Method == http.MethodGet {
		h.listInspections(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "remediations" && parts[2] == "reassign" && r.Method == http.MethodPatch {
		h.reassignRemediations(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "remediations" && r.Method == http.MethodPatch {
		h.patchRemediation(w, r, id, parts[2])
		return
	}
	if len(parts) == 2 && parts[1] == "remediations" && (r.Method == http.MethodPost || r.Method == http.MethodPatch) {
		if r.URL.Query().Get("action") != "" {
			h.changeRemediation(w, r, id)
			return
		}
		h.remediate(w, r, id)
		return
	}
	if len(parts) == 4 && parts[1] == "remediations" && parts[3] == "resolve" && r.Method == http.MethodPost {
		h.resolve(w, r, id, parts[2])
		return
	}
	if len(parts) == 2 && parts[1] == "release-requests" && r.Method == http.MethodPost {
		h.release(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "timeline" && r.Method == http.MethodGet {
		if ok, eventID, expectedHash := h.app.VerifyTimelineDiagnostic(id); !ok {
			write(w, http.StatusConflict, map[string]interface{}{"error": "integrity_error", "breakpoint_event_id": eventID, "expected_summary": expectedHash, "verified": false})
			return
		}
		ev := h.app.Timeline(id)
		q := r.URL.Query()
		if v := q.Get("action"); v != "" {
			f := ev[:0]
			for _, x := range ev {
				if x.Action == v {
					f = append(f, x)
				}
			}
			ev = f
		}
		if v := q.Get("actor"); v != "" {
			f := ev[:0]
			for _, x := range ev {
				if x.Actor == v {
					f = append(f, x)
				}
			}
			ev = f
		}
		if v := q.Get("since"); v != "" {
			t, e := time.Parse(time.RFC3339, v)
			if e != nil {
				fail(w, domain.ErrInvalid)
				return
			}
			f := ev[:0]
			for _, x := range ev {
				if !x.At.Before(t) {
					f = append(f, x)
				}
			}
			ev = f
		}
		if v := q.Get("until"); v != "" {
			t, e := time.Parse(time.RFC3339, v)
			if e != nil {
				fail(w, domain.ErrInvalid)
				return
			}
			f := ev[:0]
			for _, x := range ev {
				if !x.At.After(t) {
					f = append(f, x)
				}
			}
			ev = f
		}
		if q.Get("since") != "" && q.Get("until") != "" {
			st, _ := time.Parse(time.RFC3339, q.Get("since"))
			en, _ := time.Parse(time.RFC3339, q.Get("until"))
			if en.Before(st) {
				fail(w, domain.ErrInvalid)
				return
			}
		}
		if c := q.Get("cursor"); c != "" {
			idx := -1
			for i, x := range ev {
				if x.ID == c {
					idx = i
					break
				}
			}
			if idx < 0 {
				fail(w, domain.ErrInvalid)
				return
			}
			ev = ev[idx+1:]
		}
		limit := 20
		if q.Get("limit") != "" {
			var e error
			limit, e = strconv.Atoi(q.Get("limit"))
			if e != nil {
				fail(w, domain.ErrInvalid)
				return
			}
		}
		if limit < 1 || limit > 200 {
			fail(w, domain.ErrInvalid)
			return
		}
		out := ev
		if len(out) > limit {
			out = out[:limit]
		}
		next := ""
		if len(ev) > len(out) {
			// Cursor denotes the last returned event; the next request skips it.
			next = out[len(out)-1].ID
		}
		resp := map[string]interface{}{"events": out, "next_cursor": next}
		if q.Get("aggregate") == "true" {
			counts := map[string]map[string]int{}
			var first, last *time.Time
			for _, x := range ev {
				if counts[x.Actor] == nil {
					counts[x.Actor] = map[string]int{}
				}
				counts[x.Actor][x.Action]++
				t := x.At
				if first == nil || t.Before(*first) {
					first = &t
				}
				if last == nil || t.After(*last) {
					last = &t
				}
			}
			resp["aggregate"] = counts
			resp["total"] = len(ev)
			resp["first_at"] = first
			resp["last_at"] = last
			resp["verified"] = true
		}
		write(w, 200, resp)
		return
	}
	if len(parts) == 2 && parts[1] == "archives" && r.Method == http.MethodGet {
		p, e := h.app.GetProject(id)
		if e != nil {
			fail(w, e)
			return
		}
		if !h.app.VerifyArchives(p) {
			bad := ""
			field := "checksum"
			for _, a := range p.Archives {
				if !archiveValid(p, a) {
					bad = a.ID
					ids := make([]string, 0, len(p.Evidence))
					for _, ev := range p.Evidence {
						ids = append(ids, ev.ID+ev.SHA256)
					}
					sort.Strings(ids)
					sum := sha256.Sum256([]byte(strings.Join(ids, "|")))
					if a.EvidenceRoot != fmt.Sprintf("%x", sum) {
						field = "evidence_root"
					}
					break
				}
			}
			write(w, http.StatusConflict, map[string]interface{}{"error": "integrity_error", "archive_id": bad, "field": field})
			return
		}
		q := r.URL.Query()
		if q.Get("manifest") == "true" {
			ver := q.Get("archive_version")
			var ar *domain.ReleaseArchive
			for _, a := range p.Archives {
				if ver == "" || a.ArchiveVersion == ver {
					ar = a
					break
				}
			}
			if ar == nil {
				fail(w, domain.ErrNotFound)
				return
			}
			if !archiveValid(p, ar) {
				write(w, http.StatusConflict, map[string]interface{}{"error": "integrity_error", "archive_version": ar.ArchiveVersion, "field": "checksum", "verified": false})
				return
			}
			items := make([]map[string]interface{}, 0)
			for _, pr := range p.Procedures {
				for _, eid := range pr.EvidenceIDs {
					for _, ev := range p.Evidence {
						if ev.ID == eid && !ev.Superseded {
							items = append(items, map[string]interface{}{"procedure_id": pr.ID, "evidence_id": ev.ID, "uri": ev.URI, "sha256": ev.SHA256})
						}
					}
				}
			}
			sort.Slice(items, func(i, j int) bool {
				return items[i]["procedure_id"].(string) < items[j]["procedure_id"].(string) || items[i]["procedure_id"] == items[j]["procedure_id"] && items[i]["evidence_id"].(string) < items[j]["evidence_id"].(string)
			})
			size := 20
			if v := q.Get("page_size"); v != "" {
				size, _ = strconv.Atoi(v)
			}
			if size < 1 || size > 200 {
				fail(w, domain.ErrInvalid)
				return
			}
			start := 0
			if c := q.Get("cursor"); c != "" {
				if !strings.HasPrefix(c, ar.ArchiveVersion+":") {
					fail(w, domain.ErrInvalid)
					return
				}
				found := false
				for i, x := range items {
					if ar.ArchiveVersion+":"+x["evidence_id"].(string) == c {
						start = i + 1
						found = true
						break
					}
				}
				if !found {
					fail(w, domain.ErrInvalid)
					return
				}
			}
			end := start + size
			if end > len(items) {
				end = len(items)
			}
			next := ""
			if end < len(items) {
				next = ar.ArchiveVersion + ":" + items[end-1]["evidence_id"].(string)
			}
			write(w, 200, map[string]interface{}{"archive_version": ar.ArchiveVersion, "manifest": items[start:end], "next_cursor": next, "evidence_root": ar.EvidenceRoot, "checksum": ar.Checksum, "release_event_id": ar.ID, "verified": true})
			return
		}
		page, size := 1, 20
		var pe error
		if q.Get("page") != "" {
			page, pe = strconv.Atoi(q.Get("page"))
		}
		if pe != nil || page < 1 {
			fail(w, domain.ErrInvalid)
			return
		}
		if q.Get("page_size") != "" {
			size, pe = strconv.Atoi(q.Get("page_size"))
		}
		if pe != nil || size < 1 || size > 200 {
			fail(w, domain.ErrInvalid)
			return
		}
		items := append([]*domain.ReleaseArchive(nil), p.Archives...)
		if v := q.Get("archive_version"); v != "" {
			f := items[:0]
			for _, a := range items {
				if a.ArchiveVersion == v {
					f = append(f, a)
				}
			}
			items = f
		}
		parseArch := func(k string) (*time.Time, error) {
			if q.Get(k) == "" {
				return nil, nil
			}
			t, e := time.Parse(time.RFC3339, q.Get(k))
			return &t, e
		}
		rs, er := parseArch("released_since")
		if er != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		ru, er := parseArch("released_until")
		if er != nil || rs != nil && ru != nil && ru.Before(*rs) {
			fail(w, domain.ErrInvalid)
			return
		}
		if rs != nil || ru != nil {
			f := items[:0]
			for _, a := range items {
				if rs != nil && a.ReleasedAt.Before(*rs) || ru != nil && a.ReleasedAt.After(*ru) {
					continue
				}
				f = append(f, a)
			}
			items = f
		}
		sort.Slice(items, func(i, j int) bool { return items[i].ReleasedAt.Before(items[j].ReleasedAt) })
		total := len(items)
		start := (page - 1) * size
		if start > total {
			start = total
		}
		end := start + size
		if end > total {
			end = total
		}
		next := ""
		if end < total {
			next = items[end-1].ID
		}
		selected := items[start:end]
		views := make([]map[string]interface{}, 0, len(selected))
		evs := h.app.Timeline(id)
		releaseEvent := ""
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Action == "released_archived" {
				releaseEvent = evs[i].ID
				break
			}
		}
		for _, a := range selected {
			views = append(views, map[string]interface{}{"archive": a, "proof": map[string]interface{}{"checksum": "verified", "evidence_root": "verified", "release_event_id": releaseEvent, "verified": true}})
		}
		write(w, 200, map[string]interface{}{"archives": selected, "archive_views": views, "total": total, "page": page, "page_size": size, "next_cursor": next})
		return
	}
	write(w, 404, map[string]string{"error": "not_found"})
}
func expected(r *http.Request) int {
	v, _ := strconv.Atoi(strings.Trim(r.Header.Get("If-Match"), "\""))
	return v
}

type baselineReq struct {
	Plan                    string          `json:"plan"`
	Materials               json.RawMessage `json:"materials"`
	RiskLevel               string          `json:"risk_level"`
	Preflight               bool            `json:"preflight"`
	ValidateOnly            bool            `json:"validate_only"`
	RiskIdentification      string          `json:"risk_identification"`
	ControlMeasures         string          `json:"control_measures"`
	ResponsiblePerson       string          `json:"responsible_person"`
	EmergencyMaterials      []string        `json:"emergency_materials"`
	RequestID               string          `json:"request_id"`
	Fingerprint             string          `json:"fingerprint"`
	ConfirmationFingerprint string          `json:"confirmation_fingerprint"`
	Approver                string          `json:"approver"`
	ApprovalReason          string          `json:"approval_reason"`
}

func parseMaterials(raw json.RawMessage) (interface{}, error) {
	if len(raw) == 0 {
		return nil, domain.ErrInvalid
	}
	var entries []domain.MaterialEntry
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &entries); err == nil && (len(entries) == 0 || strings.HasPrefix(strings.TrimSpace(string(raw[1:])), "{")) {
			return entries, nil
		}
		var names []string
		if err := json.Unmarshal(raw, &names); err == nil {
			return names, nil
		}
	}
	return nil, domain.ErrInvalid
}

func (h *Handler) baseline(w http.ResponseWriter, r *http.Request, id string) {
	var q baselineReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	materials, e := parseMaterials(q.Materials)
	if e != nil {
		if !(q.Preflight || q.ValidateOnly) {
			fail(w, e)
			return
		}
		materials = nil
	}
	if q.Preflight || q.ValidateOnly || r.URL.Query().Get("preflight") == "true" {
		planText := q.Plan + " risk_identification:" + q.RiskIdentification + " control_measures:" + q.ControlMeasures + " responsible_person:" + q.ResponsiblePerson + " emergency_materials:" + strings.Join(q.EmergencyMaterials, " ")
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = q.RequestID
		}
		pf, e := h.app.BaselinePreflight(id, planText, materials, q.RiskLevel, actor(r), reqID, expected(r))
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, pf)
		return
	}
	exp := 0
	if hv := strings.Trim(r.Header.Get("If-Match"), "\""); hv != "" {
		var pe error
		exp, pe = strconv.Atoi(hv)
		if pe != nil || exp < 1 {
			fail(w, domain.ErrInvalid)
			return
		}
	} else {
		fail(w, domain.ErrInvalid)
		return
	}
	plan := q.Plan + " risk_identification:" + q.RiskIdentification + " control_measures:" + q.ControlMeasures + " responsible_person:" + q.ResponsiblePerson + " emergency_materials:" + strings.Join(q.EmergencyMaterials, " ")
	reqID := r.Header.Get("Idempotency-Key")
	if reqID == "" {
		reqID = q.RequestID
	}
	if reqID == "" {
		reqID = r.Header.Get("X-Request-ID")
	}
	fp := r.Header.Get("X-Baseline-Fingerprint")
	if fp == "" {
		fp = q.Fingerprint
	}
	if fp == "" {
		fp = q.ConfirmationFingerprint
	}
	p, e := h.app.Baseline(id, plan, materials, q.RiskLevel, actor(r), exp, reqID, fp, q.Approver, q.ApprovalReason)
	if e != nil {
		fail(w, e)
		return
	}
	b, _ := json.Marshal(p)
	var out map[string]interface{}
	_ = json.Unmarshal(b, &out)
	out["next"] = "/v1/projects/" + id + "/procedures"
	write(w, 200, out)
}

type procedureReq struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Technician string `json:"technician"`
	Sequence   int    `json:"sequence"`
}

func (h *Handler) addProcedure(w http.ResponseWriter, r *http.Request, id string) {
	b, e := io.ReadAll(r.Body)
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	var many []procedureReq
	workloadLimit := 3
	envelopeValidate := false
	if len(b) > 0 && b[0] == '{' {
		var env struct {
			Procedures    []procedureReq `json:"procedures"`
			WorkloadLimit int            `json:"workload_limit"`
			ValidateOnly  bool           `json:"validate_only"`
			RequestID     string         `json:"request_id"`
		}
		if json.Unmarshal(b, &env) == nil && len(env.Procedures) > 0 {
			many = env.Procedures
			workloadLimit = env.WorkloadLimit
			envelopeValidate = env.ValidateOnly
			if env.RequestID != "" {
				r.Header.Set("Idempotency-Key", env.RequestID)
			} else if rid := r.Header.Get("X-Request-ID"); rid != "" {
				r.Header.Set("Idempotency-Key", rid)
			}
		}
	}
	if len(many) > 0 { /* envelope decoded */
	} else {
		if len(b) > 0 && b[0] == '[' {
			d := json.NewDecoder(strings.NewReader(string(b)))
			d.DisallowUnknownFields()
			if d.Decode(&many) != nil {
				fail(w, domain.ErrInvalid)
				return
			}
		} else {
			var one procedureReq
			d := json.NewDecoder(strings.NewReader(string(b)))
			d.DisallowUnknownFields()
			if d.Decode(&one) != nil {
				fail(w, domain.ErrInvalid)
				return
			}
			many = []procedureReq{one}
		}
	}
	items := make([]*domain.ProcedureRecord, 0, len(many))
	for _, q := range many {
		items = append(items, &domain.ProcedureRecord{Name: q.Name, Technician: q.Technician, Sequence: q.Sequence, ID: q.ID})
	}
	validateOnly := envelopeValidate || r.URL.Query().Get("validate_only") == "true"
	if strings.Contains(string(b), "\"validate_only\"") {
		var meta struct {
			ValidateOnly bool `json:"validate_only"`
		}
		_ = json.Unmarshal(b, &meta)
		validateOnly = meta.ValidateOnly
	}
	if len(items) > 1 || validateOnly {
		reqID := r.Header.Get("Idempotency-Key")
		if reqID == "" {
			reqID = r.Header.Get("X-Request-ID")
		}
		rev := 0
		if hv := strings.Trim(r.Header.Get("If-Match"), "\""); hv != "" {
			rev, _ = strconv.Atoi(hv)
		}
		res, e := h.app.AddProceduresBatchLimit(id, items, actor(r), reqID, validateOnly, workloadLimit, rev, r.Header.Get("X-Schedule-Fingerprint"))
		if e != nil {
			fail(w, e)
			return
		}
		if validateOnly {
			write(w, 200, res)
		} else {
			write(w, 201, res)
		}
		return
	}
	if len(items) == 1 {
		p, e := h.app.AddProcedure(id, items[0].Name, items[0].Technician, items[0].Sequence, actor(r))
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 201, p)
		return
	}
	out, e := h.app.AddProcedures(id, items, actor(r))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, out)
}

func (h *Handler) reorder(w http.ResponseWriter, r *http.Request, id string) {
	var q struct {
		Procedures    []procedureReq `json:"procedures"`
		Handoff       string         `json:"handoff"`
		WorkloadLimit int            `json:"workload_limit"`
		Preflight     bool           `json:"preflight"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if len(q.Procedures) == 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	xs := make([]*domain.ProcedureRecord, len(q.Procedures))
	for i, x := range q.Procedures {
		xs[i] = &domain.ProcedureRecord{ID: x.ID, Name: x.Name, Technician: x.Technician, Sequence: x.Sequence}
	}
	request := r.Header.Get("Idempotency-Key")
	if request == "" {
		request = r.Header.Get("X-Request-ID")
	}
	if q.Preflight || r.URL.Query().Get("preflight") == "true" {
		p, e := h.app.GetProject(id)
		if e != nil {
			fail(w, e)
			return
		}
		loads := map[string]int{}
		affected := []string{}
		byID := map[string]*domain.ProcedureRecord{}
		for _, x := range p.Procedures {
			byID[x.ID] = x
			if !x.Completed {
				loads[x.Technician]++
			}
		}
		for _, x := range xs {
			if old := byID[x.ID]; old != nil && old.Technician != x.Technician {
				loads[old.Technician]--
				loads[x.Technician]++
				affected = append(affected, old.ID)
			}
			if old := byID[x.ID]; old != nil && old.Sequence != x.Sequence && !old.Completed {
				affected = append(affected, old.ID)
				for _, n := range p.Procedures {
					if !n.Completed && (n.Sequence == old.Sequence-1 || n.Sequence == old.Sequence+1) {
						affected = append(affected, n.ID)
					}
				}
			}
		}
		limit := q.WorkloadLimit
		if limit < 1 {
			limit = 3
		}
		alerts := []map[string]interface{}{}
		for tech, n := range loads {
			if tech != "" && n > limit {
				alerts = append(alerts, map[string]interface{}{"technician": tech, "workload": n, "limit": limit})
			}
		}
		write(w, 200, map[string]interface{}{"project_id": id, "revision": p.PlanRevision, "procedures": p.Procedures, "load_alerts": alerts, "affected_procedures": affected, "affected_technicians": loads, "request_id": request})
		return
	}
	if expected(r) <= 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	p, e := h.app.ReorderProcedures(id, xs, actor(r), request, expected(r), q.Handoff, strconv.Itoa(q.WorkloadLimit))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, map[string]interface{}{"project": p, "procedures": p.Procedures, "revision": p.PlanRevision})
}
func (h *Handler) pauseResume(w http.ResponseWriter, r *http.Request, id, pid, action string) {
	var q struct {
		Reason      string `json:"reason"`
		At          string `json:"at"`
		RequestID   string `json:"request_id"`
		Environment string `json:"environment"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if strings.TrimSpace(q.RequestID) == "" {
		q.RequestID = r.Header.Get("X-Request-ID")
	}
	if strings.TrimSpace(q.RequestID) == "" {
		q.RequestID = r.Header.Get("Idempotency-Key")
	}
	if q.At == "" {
		q.At = time.Now().UTC().Format(time.RFC3339)
	}
	at, e := time.Parse(time.RFC3339, q.At)
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	if expected(r) <= 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	if action == "start" {
		e = h.app.StartProcedure(id, pid, actor(r), q.RequestID, at, expected(r))
	} else if action == "pause" {
		e = h.app.PauseProcedure(id, pid, q.Reason, actor(r), q.RequestID, at, expected(r))
	} else {
		if strings.TrimSpace(q.Environment) == "" {
			fail(w, fmt.Errorf("environment_snapshot_required"))
			return
		}
		e = h.app.ResumeProcedure(id, pid, actor(r), q.RequestID, at, expected(r), q.Environment)
	}
	if e != nil {
		fail(w, e)
		return
	}
	p, _ := h.app.GetProject(id)
	write(w, 200, p)
}
func (h *Handler) freezeInspection(w http.ResponseWriter, r *http.Request, id, iid string) {
	var q struct {
		RequestID string `json:"request_id"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if expected(r) <= 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	in, e := h.app.FreezeInspection(id, iid, actor(r), q.RequestID, expected(r))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, in)
}

type evidenceReq struct {
	ID                string            `json:"id"`
	Kind              string            `json:"kind"`
	URI               string            `json:"uri"`
	SHA256            string            `json:"sha256"`
	CapturedAt        string            `json:"captured_at"`
	Metadata          map[string]string `json:"metadata"`
	ReplaceOf         string            `json:"replace_of,omitempty"`
	ReplacementReason string            `json:"replacement_reason,omitempty"`
}
type completeReq struct {
	StartedAt   string        `json:"started_at"`
	EndedAt     string        `json:"ended_at"`
	Environment string        `json:"environment"`
	Instruction string        `json:"instruction"`
	Result      string        `json:"result"`
	Evidence    []evidenceReq `json:"evidence"`
	RequestID   string        `json:"request_id"`
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request, id, pid string) {
	var q completeReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if strings.TrimSpace(q.RequestID) == "" {
		q.RequestID = r.Header.Get("X-Request-ID")
	}
	exp := expected(r)
	if strings.TrimSpace(q.RequestID) == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	var st time.Time
	var e error
	if strings.TrimSpace(q.StartedAt) == "" {
		p, ge := h.app.GetProject(id)
		if ge != nil {
			fail(w, ge)
			return
		}
		for _, pr := range p.Procedures {
			if pr.ID == pid && pr.StartedAt != nil {
				st = *pr.StartedAt
				break
			}
		}
		if st.IsZero() {
			fail(w, domain.ErrInvalid)
			return
		}
	} else {
		st, e = time.Parse(time.RFC3339, q.StartedAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
	}
	en, e := time.Parse(time.RFC3339, q.EndedAt)
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	ev := make([]*domain.EvidenceItem, 0, len(q.Evidence))
	for _, x := range q.Evidence {
		if len(strings.TrimSpace(x.SHA256)) != 64 || !allHex(x.SHA256) || strings.TrimSpace(x.URI) == "" || strings.TrimSpace(x.Kind) == "" || !validEvidenceMetadata(x.Kind, x.Metadata) {
			fail(w, domain.ErrInvalid)
			return
		}
		t, e := time.Parse(time.RFC3339, x.CapturedAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		eid := strings.TrimSpace(x.ID)
		if eid == "" {
			// Deterministic id makes request_id retries stable while retaining uniqueness per item.
			eid = "EV-" + strconv.FormatUint(uint64(len(ev)), 10) + "-" + x.SHA256
		}
		ev = append(ev, &domain.EvidenceItem{ID: eid, Kind: x.Kind, URI: x.URI, SHA256: x.SHA256, CapturedAt: t, Metadata: x.Metadata, ReplaceOf: x.ReplaceOf, ReplacementReason: x.ReplacementReason})
	}
	p, e := h.app.CompleteProcedure(id, pid, st, en, q.Environment, q.Instruction, q.Result, actor(r), q.RequestID, ev, exp)
	if e != nil {
		fail(w, e)
		return
	}
	var view interface{} = p
	if len(p.Procedures) > 0 {
		for _, pr := range p.Procedures {
			if pr.ID == pid {
				b, _ := json.Marshal(p)
				var m map[string]interface{}
				_ = json.Unmarshal(b, &m)
				m["normalized_environment"], m["trend_status"], m["trend_warnings"] = pr.EnvironmentParams, pr.TrendStatus, pr.TrendWarnings
				m["effective_work"] = pr.EffectiveWork.String()
				m["effective_work_ns"] = pr.EffectiveWork
				view = m
				break
			}
		}
	}
	write(w, 200, view)
}

func (h *Handler) listEvidence(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	var since, until *time.Time
	var e error
	if q.Get("captured_since") != "" {
		t, er := time.Parse(time.RFC3339, q.Get("captured_since"))
		e = er
		since = &t
	}
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	if q.Get("captured_until") != "" {
		t, er := time.Parse(time.RFC3339, q.Get("captured_until"))
		e = er
		until = &t
	}
	if e != nil || since != nil && until != nil && until.Before(*since) {
		fail(w, domain.ErrInvalid)
		return
	}
	size := 20
	if q.Get("page_size") != "" {
		size, e = strconv.Atoi(q.Get("page_size"))
	}
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	out, e := h.app.ListEvidence(id, q.Get("procedure_id"), q.Get("kind"), since, until, size, q.Get("cursor"), actor(r), r.Header.Get("X-Request-ID"))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, out)
}

func (h *Handler) reviseInspection(w http.ResponseWriter, r *http.Request, id, iid string) {
	var q inspectReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	var due *time.Time
	if q.DueAt != "" {
		t, e := time.Parse(time.RFC3339, q.DueAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		due = &t
	}
	rev := expected(r)
	req := r.Header.Get("Idempotency-Key")
	if req == "" {
		req = q.RequestID
	}
	x, e := h.app.ReviseInspection(id, iid, rev, q.Inspector, q.Decision, q.Findings, due, q.SampledProcedureIDs, q.EvidenceIDs, actor(r), req)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, x)
}

type inspectReq struct {
	Inspector           string   `json:"inspector"`
	Decision            string   `json:"decision"`
	Findings            []string `json:"findings"`
	DueAt               string   `json:"due_at"`
	RequestID           string   `json:"request_id"`
	SampledProcedureIDs []string `json:"sampled_procedure_ids"`
	EvidenceIDs         []string `json:"evidence_ids"`
}

func (h *Handler) inspect(w http.ResponseWriter, r *http.Request, id string) {
	var q inspectReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	var due *time.Time
	if q.DueAt != "" {
		t, e := time.Parse(time.RFC3339, q.DueAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		due = &t
	}
	request := r.Header.Get("Idempotency-Key")
	if request == "" {
		request = q.RequestID
	}
	if strings.TrimSpace(request) == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	if expected(r) <= 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	i, e := h.app.Inspect(id, q.Inspector, q.Decision, q.Findings, due, actor(r), request, q.SampledProcedureIDs, q.EvidenceIDs, expected(r))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, i)
}
func (h *Handler) listInspections(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	if q.Get("aggregate") == "defects" {
		var since, until *time.Time
		var e error
		if q.Get("since") != "" {
			t, er := time.Parse(time.RFC3339, q.Get("since"))
			if er != nil {
				fail(w, domain.ErrInvalid)
				return
			}
			since = &t
		}
		if q.Get("until") != "" {
			t, er := time.Parse(time.RFC3339, q.Get("until"))
			if er != nil {
				fail(w, domain.ErrInvalid)
				return
			}
			until = &t
		}
		if since != nil && until != nil && until.Before(*since) {
			fail(w, domain.ErrInvalid)
			return
		}
		out, e := h.app.DefectAggregate(id, q.Get("decision"), q.Get("procedure_id"), since, until)
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, out)
		return
	}
	decision, ins := q.Get("decision"), q.Get("inspector")
	if decision != "" && decision != "pass" && decision != "remediate" && decision != "fail" {
		fail(w, domain.ErrInvalid)
		return
	}
	page, size := 1, 20
	var e error
	if q.Get("page") != "" {
		page, e = strconv.Atoi(q.Get("page"))
	}
	if e != nil || page < 1 {
		fail(w, domain.ErrInvalid)
		return
	}
	if q.Get("page_size") != "" {
		size, e = strconv.Atoi(q.Get("page_size"))
	}
	if e != nil || size < 1 || size > 200 {
		fail(w, domain.ErrInvalid)
		return
	}
	parse := func(k string) (*time.Time, error) {
		if q.Get(k) == "" {
			return nil, nil
		}
		t, e := time.Parse(time.RFC3339, q.Get(k))
		return &t, e
	}
	since, e := parse("since")
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	until, e := parse("until")
	if e != nil || since != nil && until != nil && until.Before(*since) {
		fail(w, domain.ErrInvalid)
		return
	}
	items, total, stats, e := h.app.ListInspections(id, decision, ins, since, until, page, size)
	if e != nil {
		fail(w, e)
		return
	}
	views := make([]map[string]interface{}, 0, len(items))
	now := time.Now()
	for _, it := range items {
		v := map[string]interface{}{"id": it.ID, "project_id": it.ProjectID, "inspector": it.Inspector, "checked_at": it.CheckedAt, "decision": it.Decision, "findings": it.Findings, "due_at": it.DueAt, "revision": it.Revision}
		if it.Decision == "remediate" {
			st := "missing_due"
			if it.DueAt != nil {
				if it.DueAt.Before(now) {
					st = "overdue"
				} else if it.DueAt.Sub(now) <= 24*time.Hour {
					st = "due"
				} else {
					st = "open"
				}
			}
			v["remediation_status"] = st
		}
		views = append(views, v)
	}
	write(w, 200, map[string]interface{}{"page": page, "page_size": size, "total": total, "inspections": views, "stats": stats})
}

type remediateReq struct {
	InspectionID string `json:"inspection_id"`
	Description  string `json:"description"`
	Assignee     string `json:"assignee"`
	DueAt        string `json:"due_at"`
}

func (h *Handler) changeRemediation(w http.ResponseWriter, r *http.Request, id string) {
	var q struct {
		ID        string `json:"id"`
		Action    string `json:"action"`
		Assignee  string `json:"assignee"`
		Reason    string `json:"reason"`
		Approver  string `json:"approver"`
		DueAt     string `json:"due_at"`
		RequestID string `json:"request_id"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if q.ID == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	var due *time.Time
	if q.DueAt != "" {
		t, e := time.Parse(time.RFC3339, q.DueAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		due = &t
	}
	x, e := h.app.ChangeRemediation(id, q.ID, q.Action, q.Assignee, q.Reason, q.Approver, actor(r), q.RequestID, due)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, x)
}

func (h *Handler) patchRemediation(w http.ResponseWriter, r *http.Request, projectID, remediationID string) {
	var q struct {
		Action    string `json:"action"`
		Assignee  string `json:"assignee"`
		Reason    string `json:"reason"`
		Approver  string `json:"approver"`
		DueAt     string `json:"due_at"`
		RequestID string `json:"request_id"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	var due *time.Time
	if q.DueAt != "" {
		t, e := time.Parse(time.RFC3339, q.DueAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		due = &t
	}
	x, e := h.app.ChangeRemediation(projectID, remediationID, q.Action, q.Assignee, q.Reason, q.Approver, actor(r), q.RequestID, due)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, x)
}

func (h *Handler) reassignRemediations(w http.ResponseWriter, r *http.Request, id string) {
	var q struct {
		IDs            []string `json:"ids"`
		RemediationIDs []string `json:"remediation_ids"`
		Assignee       string   `json:"assignee"`
		Reason         string   `json:"reason"`
		RequestID      string   `json:"request_id"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if len(q.IDs) == 0 {
		q.IDs = q.RemediationIDs
	}
	if len(q.IDs) == 0 || strings.TrimSpace(q.Assignee) == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	req := q.RequestID
	if req == "" {
		req = r.Header.Get("Idempotency-Key")
	}
	p, e := h.app.ReassignRemediations(id, q.IDs, q.Assignee, actor(r), req, q.Reason)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, map[string]interface{}{"project": p, "remediations": p.Remediations})
}

func (h *Handler) remediate(w http.ResponseWriter, r *http.Request, id string) {
	var q remediateReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	var due *time.Time
	if q.DueAt != "" {
		t, e := time.Parse(time.RFC3339, q.DueAt)
		if e != nil {
			fail(w, domain.ErrInvalid)
			return
		}
		due = &t
	}
	x, e := h.app.Remediate(id, q.InspectionID, q.Description, q.Assignee, due, actor(r))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, x)
}

type resolveReq struct {
	EvidenceIDs []string `json:"evidence_ids"`
	Reviewer    string   `json:"reviewer"`
	Decision    string   `json:"decision"`
	Reason      string   `json:"reason"`
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, id, rid string) {
	var q resolveReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	decision := q.Decision
	if decision == "" {
		decision = "approve"
	}
	p, e := h.app.ResolveDecision(id, rid, decision, q.Reason, q.EvidenceIDs, q.Reviewer, actor(r), r.Header.Get("Idempotency-Key"))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, p)
}

func (h *Handler) listRemediations(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	page, size := 1, 20
	var e error
	if q.Get("page") != "" {
		page, e = strconv.Atoi(q.Get("page"))
	}
	if e != nil || page < 1 {
		fail(w, domain.ErrInvalid)
		return
	}
	if q.Get("page_size") != "" {
		size, e = strconv.Atoi(q.Get("page_size"))
	}
	if e != nil || size < 1 || size > 200 {
		fail(w, domain.ErrInvalid)
		return
	}
	status := q.Get("status")
	if status != "" && status != "open" && status != "closed" && status != "overdue" {
		fail(w, domain.ErrInvalid)
		return
	}
	parse := func(k string) (*time.Time, error) {
		if q.Get(k) == "" {
			return nil, nil
		}
		t, e := time.Parse(time.RFC3339, q.Get(k))
		return &t, e
	}
	since, e := parse("due_since")
	if e != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	until, e := parse("due_until")
	if e != nil || since != nil && until != nil && until.Before(*since) {
		fail(w, domain.ErrInvalid)
		return
	}
	items, total, stats, e := h.app.ListRemediations(id, q.Get("assignee"), status, since, until, page, size)
	if e != nil {
		fail(w, e)
		return
	}
	resp := map[string]interface{}{"remediations": items, "total": total, "page": page, "page_size": size, "stats": stats}
	if q.Get("include_sla") == "true" {
		by := map[string]int{}
		escalated := 0
		overdue := 0
		now := time.Now()
		for _, r := range items {
			level := "normal"
			if r.EscalationRequired {
				level = "escalated"
				escalated++
			}
			if r.DueAt != nil && r.DueAt.Before(now) {
				overdue++
			}
			by[level]++
		}
		resp["sla_summary"] = map[string]interface{}{"by_level": by, "escalated": escalated, "overdue": overdue}
	}
	write(w, 200, resp)
}

func (h *Handler) resolveBatch(w http.ResponseWriter, r *http.Request, id string) {
	var q struct {
		Reviewer string `json:"reviewer"`
		Items    []struct {
			ID       string   `json:"id"`
			Evidence []string `json:"evidence_ids"`
		} `json:"items"`
		Remediations []struct {
			ID       string   `json:"id"`
			Evidence []string `json:"evidence_ids"`
		} `json:"remediations"`
	}
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if len(q.Items) == 0 {
		q.Items = q.Remediations
	}
	if strings.TrimSpace(q.Reviewer) == "" || len(q.Items) == 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	xs := make([]struct {
		ID       string
		Evidence []string
	}, len(q.Items))
	for i, x := range q.Items {
		xs[i].ID = x.ID
		xs[i].Evidence = x.Evidence
	}
	p, e := h.app.ResolveBatch(id, xs, q.Reviewer, actor(r))
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 200, p)
}

type releaseReq struct {
	Reviewers         []string               `json:"reviewers"`
	Opinions          map[string]string      `json:"opinions"`
	RequestID         string                 `json:"request_id"`
	Preflight         bool                   `json:"preflight"`
	ReportFingerprint string                 `json:"report_fingerprint"`
	OpinionReason     json.RawMessage        `json:"opinion_reason"`
	Recusal           []string               `json:"recusal"`
	QuorumReport      map[string]interface{} `json:"quorum_report"`
	ReviewerRoles     map[string]string      `json:"reviewer_roles,omitempty"`
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request, id string) {
	var q releaseReq
	if e := decode(r, &q); e != nil {
		fail(w, e)
		return
	}
	if q.Preflight || r.URL.Query().Get("preflight") == "true" {
		out, e := h.app.ReleasePreflight(id, actor(r), q.RequestID)
		if e != nil {
			fail(w, e)
			return
		}
		write(w, 200, out)
		return
	}
	if strings.TrimSpace(q.RequestID) == "" || len(q.Reviewers) < 2 {
		q.RequestID = r.Header.Get("Idempotency-Key")
		if strings.TrimSpace(q.RequestID) == "" || len(q.Reviewers) < 2 {
			fail(w, domain.ErrInvalid)
			return
		}
	}
	reasonText := ""
	if len(q.OpinionReason) > 0 {
		var s string
		if json.Unmarshal(q.OpinionReason, &s) == nil {
			reasonText = s
		} else {
			var m map[string]string
			if json.Unmarshal(q.OpinionReason, &m) == nil {
				for _, v := range m {
					reasonText = v
					break
				}
			}
		}
	}
	for _, opinion := range q.Opinions {
		op := strings.ToLower(strings.TrimSpace(opinion))
		if (op == "reject" || op == "abstain" || op == "拒绝" || op == "弃权") && strings.TrimSpace(reasonText) == "" {
			fail(w, domain.ErrInvalid)
			return
		}
	}
	if len(q.ReviewerRoles) > 0 {
		p, _ := h.app.GetProject(id)
		need := "quality"
		if p != nil && p.RiskLevel == "high" {
			need = "protection"
		}
		ok := false
		for _, r := range q.Reviewers {
			role := strings.ToLower(strings.TrimSpace(q.ReviewerRoles[r]))
			if need == "protection" && (strings.Contains(role, "protection") || strings.Contains(role, "文物保护")) || need == "quality" && (strings.Contains(role, "quality") || strings.Contains(role, "质量")) {
				ok = true
			}
		}
		if !ok {
			fail(w, &domain.ProjectConflictError{Reason: "缺少必需专家角色", ProjectID: id})
			return
		}
	}
	a, e := h.app.ReleaseWithReport(id, q.Reviewers, q.Opinions, actor(r), q.RequestID, q.ReportFingerprint)
	if e != nil {
		fail(w, e)
		return
	}
	write(w, 201, a)
}
