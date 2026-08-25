package application

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"restoration-quality/internal/domain"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Service) ReorderProcedures(id string, items []*domain.ProcedureRecord, actor, request string, expected int, handoffs ...string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if expected > 0 && p.PlanRevision != expected {
		return nil, domain.ErrConflict
	}
	handoff := ""
	workloadLimit := 3
	if len(handoffs) > 0 {
		handoff = strings.TrimSpace(handoffs[0])
	}
	if len(handoffs) > 1 {
		if n, err := strconv.Atoi(strings.TrimSpace(handoffs[1])); err == nil && n > 0 {
			workloadLimit = n
		}
	}
	byID := map[string]*domain.ProcedureRecord{}
	for _, old := range p.Procedures {
		byID[old.ID] = old
	}
	load := map[string]int{}
	for _, x := range items {
		old := byID[x.ID]
		if old != nil && (old.StartedAt != nil && old.Sequence != x.Sequence || old.Technician != x.Technician) && handoff == "" {
			return nil, domain.ErrConflict
		}
		if old == nil || !old.Completed {
			load[strings.TrimSpace(x.Technician)]++
		}
	}
	for tech, n := range load {
		if tech != "" && n > workloadLimit {
			return nil, &domain.CapacityError{Technician: tech, Limit: workloadLimit, Count: n}
		}
	}
	b, _ := json.Marshal(items)
	fp := fmt.Sprintf("%x", sha256.Sum256(b))
	if request != "" {
		if v, ok := s.idem["reorder:"+request]; ok {
			old := v.(struct {
				fp string
				p  *domain.RestorationProject
			})
			if old.fp != fp {
				return nil, domain.ErrConflict
			}
			return old.p, nil
		}
	}
	if e = p.ReorderProcedures(items, s.now()); e != nil {
		return nil, e
	}
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "procedure_reordered", actor, request, map[string]interface{}{"fingerprint": fp, "revision": p.PlanRevision, "handoff": handoff})
	if request != "" {
		s.idem["reorder:"+request] = struct {
			fp string
			p  *domain.RestorationProject
		}{fp, p}
	}
	return p, nil
}
func (s *Service) PauseProcedure(id, pid, reason, actor, request string, at time.Time, expected int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request != "" {
		key := "procedure_pause:" + request
		fp := fmt.Sprintf("%s|%s|%s", id, pid, at.UTC().Format(time.RFC3339Nano)) + "|" + strings.TrimSpace(reason)
		if v, ok := s.idem[key]; ok {
			if v != fp {
				return domain.ErrConflict
			}
			return nil
		}
		_ = key
	}
	p, e := s.store.Get(id)
	if e != nil {
		return e
	}
	if expected > 0 && p.PlanRevision != expected {
		return domain.ErrConflict
	}
	if e = p.PauseProcedure(pid, reason, actor, at); e != nil {
		return e
	}
	if e = s.store.Update(p, 0); e != nil {
		return e
	}
	s.event(id, "procedure_paused", actor, request, map[string]interface{}{"procedure_id": pid, "reason": reason})
	if request != "" {
		s.idem["procedure_pause:"+request] = fmt.Sprintf("%s|%s|%s|%s", id, pid, at.UTC().Format(time.RFC3339Nano), strings.TrimSpace(reason))
	}
	return nil
}

func (s *Service) StartProcedure(id, pid, actor, request string, at time.Time, expected int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request != "" {
		key := "procedure_start:" + request
		fp := fmt.Sprintf("%s|%s|%s", id, pid, at.UTC().Format(time.RFC3339Nano))
		if v, ok := s.idem[key]; ok {
			if v != fp {
				return domain.ErrConflict
			}
			return nil
		}
		_ = key
	}
	p, e := s.store.Get(id)
	if e != nil {
		return e
	}
	if expected > 0 && p.PlanRevision != expected {
		return domain.ErrConflict
	}
	for _, pr := range p.Procedures {
		if pr.ID != pid {
			continue
		}
		if pr.Completed || pr.StartedAt != nil {
			return domain.ErrConflict
		}
		if pr.Sequence > 1 {
			for _, prev := range p.Procedures {
				if prev.Sequence == pr.Sequence-1 && !prev.Completed {
					return domain.ErrConflict
				}
			}
		}
		pr.StartedAt = &at
		p.PlanRevision++
		p.UpdatedAt = at
		if e = s.store.Update(p, 0); e != nil {
			return e
		}
		s.event(id, "procedure_started", actor, request, map[string]interface{}{"procedure_id": pid})
		if request != "" {
			s.idem["procedure_start:"+request] = fmt.Sprintf("%s|%s|%s", id, pid, at.UTC().Format(time.RFC3339Nano))
		}
		return nil
	}
	return domain.ErrNotFound
}
func (s *Service) ResumeProcedure(id, pid, actor, request string, at time.Time, expected int, environments ...string) error {
	env := ""
	if len(environments) > 0 {
		env = environments[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if request != "" {
		key := "procedure_resume:" + request
		fp := fmt.Sprintf("%s|%s|%s|%s", id, pid, at.UTC().Format(time.RFC3339Nano), env)
		if v, ok := s.idem[key]; ok {
			if v != fp {
				return domain.ErrConflict
			}
			return nil
		}
		_ = key
	}
	p, e := s.store.Get(id)
	if e != nil {
		return e
	}
	if expected > 0 && p.PlanRevision != expected {
		return domain.ErrConflict
	}
	if env != "" {
		e = p.ResumeProcedureWithEnvironment(pid, actor, at, env)
	} else {
		e = p.ResumeProcedure(pid, actor, at)
	}
	if e != nil {
		return e
	}
	if e = s.store.Update(p, 0); e != nil {
		return e
	}
	s.event(id, "procedure_resumed", actor, request, map[string]interface{}{"procedure_id": pid})
	if request != "" {
		s.idem["procedure_resume:"+request] = fmt.Sprintf("%s|%s|%s|%s", id, pid, at.UTC().Format(time.RFC3339Nano), env)
	}
	return nil
}
func (s *Service) FreezeInspection(id, iid, actor, request string, expected int) (*domain.InspectionBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if expected > 0 && p.PlanRevision != expected {
		return nil, domain.ErrConflict
	}
	var in *domain.InspectionBatch
	for _, x := range p.Inspections {
		if x.ID == iid {
			in = x
		}
	}
	if in == nil {
		return nil, domain.ErrNotFound
	}
	if in.Frozen {
		return nil, domain.ErrConflict
	}
	if e = p.FreezeInspection(iid, s.now()); e != nil {
		return nil, e
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "inspection_frozen", actor, request, map[string]interface{}{"inspection_id": iid, "coverage": in.Coverage})
	return in, nil
}

func (s *Service) ChangeRemediation(id, rid, action, assignee, reason, approver, actor, request string, due *time.Time) (*domain.Remediation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	var r *domain.Remediation
	for _, x := range p.Remediations {
		if x.ID == rid {
			r = x
		}
	}
	if r == nil {
		return nil, domain.ErrNotFound
	}
	if r.Status == "closed" {
		return nil, domain.ErrConflict
	}
	if action == "escalate" {
		if r.EscalatedAt != nil {
			return nil, domain.ErrConflict
		}
		now := s.now()
		r.EscalatedAt = &now
		if r.Status == "open" {
			r.Status = "overdue"
		}
	}
	if action == "change_due" {
		if due == nil || strings.TrimSpace(reason) == "" || strings.TrimSpace(approver) == "" || due.Before(s.now()) {
			return nil, domain.ErrInvalid
		}
		if r.OriginalDueAt == nil && r.DueAt != nil {
			t := *r.DueAt
			r.OriginalDueAt = &t
		}
		// risk-based extension ceiling and inspection deadline
		limit := 1
		if p.RiskLevel == "medium" {
			limit = 2
		}
		if p.RiskLevel == "high" {
			limit = 1
		}
		if r.ExtensionCount >= limit {
			return nil, domain.ErrConflict
		}
		for _, in := range p.Inspections {
			if in.ID == r.InspectionID && in.DueAt != nil && due.After(*in.DueAt) {
				return nil, domain.ErrConflict
			}
		}
		r.DueAt = due
		r.ExtensionCount++
		if r.ExtensionCount >= limit {
			r.EscalationRequired = true
		}
		r.ExtensionReason = reason
		r.ApprovedBy = approver
	}
	if action == "reassign" {
		if strings.TrimSpace(assignee) == "" || strings.TrimSpace(assignee) == strings.TrimSpace(r.Assignee) || strings.TrimSpace(reason) == "" {
			return nil, domain.ErrInvalid
		}
		if r.OriginalAssignee == "" {
			r.OriginalAssignee = r.Assignee
		}
		r.Assignee = strings.TrimSpace(assignee)
		if r.EscalatedAt == nil && r.DueAt != nil && r.DueAt.Before(s.now()) {
			t := s.now()
			r.EscalatedAt = &t
		}
	}
	if action != "change_due" && action != "reassign" && action != "escalate" {
		return nil, domain.ErrInvalid
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "remediation_"+action, actor, request, map[string]interface{}{"remediation_id": rid, "reason": reason, "approved_by": approver})
	return r, nil
}

func (s *Service) ReassignRemediations(projectID string, ids []string, assignee, actor, request string, reasons ...string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normIDs := append([]string(nil), ids...)
	sort.Strings(normIDs)
	fb, _ := json.Marshal(struct {
		IDs      []string
		Assignee string
	}{normIDs, strings.TrimSpace(assignee)})
	fp := fmt.Sprintf("%x", sha256.Sum256(fb))
	if request != "" {
		if v, ok := s.idem["reassign:"+request]; ok {
			if rec, ok := v.(struct {
				fp string
				p  *domain.RestorationProject
			}); ok {
				if rec.fp != fp {
					return nil, domain.ErrConflict
				}
				return rec.p, nil
			}
		}
	}
	p, e := s.store.Get(projectID)
	if e != nil {
		return nil, e
	}
	reason := ""
	if len(reasons) > 0 {
		reason = strings.TrimSpace(reasons[0])
	}
	if reason == "" {
		return nil, domain.ErrInvalid
	}
	seen := map[string]bool{}
	for _, rid := range ids {
		if seen[rid] {
			return nil, domain.ErrInvalid
		}
		seen[rid] = true
		var found *domain.Remediation
		for _, r := range p.Remediations {
			if r.ID == rid {
				found = r
			}
		}
		if found == nil || found.Status == "closed" || strings.TrimSpace(assignee) == "" {
			return nil, domain.ErrConflict
		}
	}
	for _, rid := range ids {
		for _, r := range p.Remediations {
			if r.ID == rid {
				if r.OriginalAssignee == "" {
					r.OriginalAssignee = r.Assignee
				}
				r.Assignee = assignee
				r.ExtensionReason = reason
				if r.DueAt != nil && r.DueAt.Before(s.now()) && r.EscalatedAt == nil {
					t := s.now()
					r.EscalatedAt = &t
				}
			}
		}
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(projectID, "remediation_batch_reassign", actor, request, map[string]interface{}{"count": len(ids), "assignee": assignee})
	if request != "" {
		s.idem["reassign:"+request] = struct {
			fp string
			p  *domain.RestorationProject
		}{fp: fp, p: p}
	}
	return p, nil
}

func (s *Service) BaselineHistory(id string, revision int) ([]domain.BaselineRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached, ok := s.baselineHistoryCache[id]; ok {
		return selectBaselineHistory(cached, revision), nil
	}
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	s.baselineHistoryCache[id] = append([]domain.BaselineRevision(nil), p.BaselineHistory...)
	return selectBaselineHistory(s.baselineHistoryCache[id], revision), nil
}

func selectBaselineHistory(history []domain.BaselineRevision, revision int) []domain.BaselineRevision {
	if revision <= 0 {
		return append([]domain.BaselineRevision(nil), history...)
	}
	out := []domain.BaselineRevision{}
	for _, x := range history {
		if x.Revision == revision {
			out = append(out, x)
		}
	}
	return out
}

// RollbackBaseline restores a historical, not-yet-executed baseline as a new revision.
func (s *Service) RollbackBaseline(id string, target, expected int, actor, request string, reasons ...string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fingerprint := fmt.Sprintf("%s|%d", id, target)
	if request != "" {
		if v, ok := s.idem["baseline_rollback:"+request]; ok {
			if rec, ok := v.(struct {
				fp string
				p  *domain.RestorationProject
			}); ok {
				if rec.fp != fingerprint {
					return nil, domain.ErrConflict
				}
				return rec.p, nil
			}
		}
	}
	p, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	if expected > 0 && p.PlanRevision != expected {
		return nil, domain.ErrConflict
	}
	for _, pr := range p.Procedures {
		if pr.StartedAt != nil {
			return nil, domain.ErrConflict
		}
	}
	var snap *domain.BaselineRevision
	for i := range p.BaselineHistory {
		if p.BaselineHistory[i].Revision == target {
			x := p.BaselineHistory[i]
			snap = &x
			break
		}
	}
	if snap == nil {
		return nil, domain.ErrNotFound
	}
	p.Plan, p.Materials, p.RiskLevel = snap.Plan, append([]domain.MaterialEntry(nil), snap.Materials...), snap.RiskLevel
	p.PlanRevision++
	reason := ""
	if len(reasons) > 0 {
		reason = strings.TrimSpace(reasons[0])
	}
	p.BaselineHistory = append(p.BaselineHistory, domain.BaselineRevision{Revision: p.PlanRevision, Plan: p.Plan, Materials: append([]domain.MaterialEntry(nil), p.Materials...), RiskLevel: p.RiskLevel, Operator: actor, At: s.now(), ImpactReason: reason})
	if err = s.store.Update(p, expected); err != nil {
		return nil, err
	}
	s.event(id, "baseline_rollback", actor, request, map[string]interface{}{"source_revision": target, "revision": p.PlanRevision})
	if request != "" {
		s.idem["baseline_rollback:"+request] = struct {
			fp string
			p  *domain.RestorationProject
		}{fingerprint, p}
	}
	return p, nil
}

var _ = sort.Strings

func (s *Service) GetByAsset(asset, actor, request string) (*domain.RestorationProject, error) {
	p, e := s.store.FindByAsset(asset)
	if e == nil {
		s.event(p.ID, "project_search_exact", actor, request, map[string]interface{}{"asset_code": strings.ToUpper(strings.TrimSpace(asset)), "match": "asset_code_exact"})
	}
	return p, e
}
