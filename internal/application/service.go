package application

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"restoration-quality/internal/audit"
	"restoration-quality/internal/domain"
	"restoration-quality/internal/persistence"
	"sort"
	"strings"
	"sync"
	"time"
)

type Service struct {
	store        *persistence.Store
	audit        *audit.Logger
	mu           sync.Mutex
	idem         map[string]interface{}
	preflight    map[string]string
	reservations map[string]reservation
	now          func() time.Time
}
type reservation struct {
	Token, Asset, Fingerprint, Candidate string
	ExpiresAt                            time.Time
	Consumed                             bool
}

type ProcedureBatchResult struct {
	Procedures  []*domain.ProcedureRecord `json:"procedures"`
	Warnings    []string                  `json:"warnings,omitempty"`
	Errors      []map[string]string       `json:"errors,omitempty"`
	Fingerprint string                    `json:"fingerprint"`
	Valid       bool                      `json:"valid"`
	Revision    int                       `json:"revision"`
	LoadAlerts  []string                  `json:"load_alerts,omitempty"`
}

type ProjectPreflight struct {
	CanCreate        bool                 `json:"can_create"`
	ProjectID        string               `json:"project_id,omitempty"`
	Status           domain.ProjectStatus `json:"status,omitempty"`
	Revision         int                  `json:"revision,omitempty"`
	Fingerprint      string               `json:"fingerprint"`
	Next             string               `json:"next"`
	ReservationToken string               `json:"reservation_token,omitempty"`
	CandidateID      string               `json:"candidate_id,omitempty"`
	ExpiresAt        time.Time            `json:"expires_at,omitempty"`
	Occupied         bool                 `json:"occupied,omitempty"`
	ConflictReason   string               `json:"conflict_reason,omitempty"`
	RetryFingerprint string               `json:"retry_fingerprint,omitempty"`
}

func New(store *persistence.Store, logger *audit.Logger) *Service {
	return &Service{store: store, audit: logger, idem: map[string]interface{}{}, preflight: map[string]string{}, reservations: map[string]reservation{}, now: time.Now}
}
func (s *Service) event(p, action, actor, request string, data map[string]interface{}) {
	_ = s.audit.Append(audit.Event{ID: fmt.Sprintf("evt-%d", s.now().UnixNano()), ProjectID: p, Action: action, Actor: actor, RequestID: request, At: s.now(), Data: data})
}
func (s *Service) CreateProject(asset, title, custodian, actor, request string, reservationToken ...string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ve error
	asset, title, custodian, ve = domain.ValidateProjectFields(asset, title, custodian)
	if ve != nil || strings.TrimSpace(request) == "" {
		if ve != nil {
			return nil, ve
		}
		return nil, domain.ErrInvalid
	}
	fb, _ := json.Marshal(struct{ Asset, Title, Custodian string }{strings.ToUpper(asset), title, custodian})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fb))
	// Resolve durable idempotency before checking a one-shot reservation token:
	// retries after a successful commit must return the original project.
	if old, e := s.store.FindByRequest(request); e == nil {
		if old.CreateFingerprint == fingerprint {
			s.event(old.ID, "project_create_idempotent", actor, request, map[string]interface{}{"plan_revision": old.PlanRevision})
			return old, nil
		}
		s.event(old.ID, "project_create_conflict", actor, request, map[string]interface{}{"reason": "request_id_reused"})
		return nil, &domain.ProjectConflictError{Reason: "request_id 已用于不同文物信息", ProjectID: old.ID}
	}
	provided := ""
	if len(reservationToken) > 0 {
		provided = strings.TrimSpace(reservationToken[0])
	}
	if r, exists := s.reservations[request]; exists {
		if provided == "" || r.Token != provided || r.Fingerprint != fingerprint || r.Asset != strings.ToUpper(asset) || r.Consumed || !s.now().Before(r.ExpiresAt) {
			s.event("", "project_reservation_conflict", actor, request, map[string]interface{}{"asset_code": asset})
			return nil, &domain.ProjectConflictError{Reason: "编号预约令牌无效或已过期"}
		}
	} else if provided != "" {
		// A supplied token must always resolve to the same live reservation;
		// silently ignoring it would turn an expired reservation into a write.
		s.event("", "project_reservation_conflict", actor, request, map[string]interface{}{"reason": "reservation_not_found"})
		return nil, &domain.ProjectConflictError{Reason: "编号预约令牌无效或已过期"}
	}
	if old, e := s.store.FindByAsset(asset); e == nil {
		s.event(old.ID, "project_create_conflict", actor, request, map[string]interface{}{"reason": "asset_code_duplicate"})
		return nil, &domain.ProjectConflictError{Reason: "文物编号已登记", ProjectID: old.ID}
	}
	id := fmt.Sprintf("RP-%s-%d", s.now().Format("20060102"), s.now().UnixNano()%1000000)
	if r, ok := s.reservations[request]; ok {
		id = r.Candidate
	}
	p, e := domain.NewProject(id, asset, title, custodian, s.now())
	if e != nil {
		return nil, e
	}
	p.CreateRequestID = request
	p.CreateFingerprint = fingerprint
	if e = s.store.Create(p); e != nil {
		return nil, e
	}
	if r, ok := s.reservations[request]; ok {
		r.Consumed = true
		s.reservations[request] = r
	}
	s.event(id, "project_created", actor, request, nil)
	s.idem[request] = p
	return p, nil
}

type BatchProjectInput struct{ AssetCode, Title, Custodian, RequestID string }
type BatchProjectResult struct {
	Projects    []*domain.RestorationProject `json:"projects"`
	Fingerprint string                       `json:"fingerprint"`
	Results     []map[string]interface{}     `json:"results"`
}

// CreateProjectsBatch validates and persists a complete batch under one application lock.
func (s *Service) CreateProjectsBatch(items []BatchProjectInput, actor, idemKey string) (BatchProjectResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(items) == 0 || len(items) > 100 || strings.TrimSpace(idemKey) == "" {
		return BatchProjectResult{}, domain.ErrInvalid
	}
	type norm struct{ a, t, c, r, fp string }
	ns := make([]norm, len(items))
	existing := make([]*domain.RestorationProject, len(items))
	seenAsset := map[string]int{}
	seenReq := map[string]int{}
	for i, in := range items {
		a, t, c, e := domain.ValidateProjectFields(in.AssetCode, in.Title, in.Custodian)
		if e != nil {
			return BatchProjectResult{Results: []map[string]interface{}{{"line": i + 1, "error": e.Error()}}}, e
		}
		r := strings.TrimSpace(in.RequestID)
		if r == "" {
			return BatchProjectResult{}, domain.ErrInvalid
		}
		key := strings.ToUpper(a)
		if j, ok := seenAsset[key]; ok {
			return BatchProjectResult{Results: []map[string]interface{}{{"line": i + 1, "field": "asset_code", "conflict_line": j + 1}}}, domain.ErrConflict
		}
		seenAsset[key] = i
		if j, ok := seenReq[r]; ok {
			return BatchProjectResult{Results: []map[string]interface{}{{"line": i + 1, "field": "request_id", "conflict_line": j + 1}}}, domain.ErrConflict
		}
		seenReq[r] = i
		fb, _ := json.Marshal(struct{ A, T, C, R string }{key, t, c, r})
		ns[i] = norm{a, t, c, r, fmt.Sprintf("%x", sha256.Sum256(fb))}
		if old, e := s.store.FindByRequest(r); e == nil {
			if old.CreateFingerprint != ns[i].fp {
				return BatchProjectResult{}, domain.ErrConflict
			}
			existing[i] = old
		}
		if existing[i] == nil {
			if old, e := s.store.FindByAsset(a); e == nil {
				return BatchProjectResult{Results: []map[string]interface{}{{"line": i + 1, "field": "asset_code", "project_id": old.ID}}}, domain.ErrConflict
			}
		}
	}
	fb, _ := json.Marshal(ns)
	fp := fmt.Sprintf("%x", sha256.Sum256(fb))
	idem := "project_batch:" + idemKey
	if v, ok := s.idem[idem]; ok {
		old := v.(BatchProjectResult)
		if old.Fingerprint != fp {
			return BatchProjectResult{}, domain.ErrConflict
		}
		return old, nil
	}
	allExisting := true
	for _, p := range existing {
		if p == nil {
			allExisting = false
			break
		}
	}
	// A batch retry is atomic: mixing already committed rows with new rows
	// under a fresh idempotency key would otherwise create duplicate assets.
	for i, p := range existing {
		if p != nil && !allExisting {
			return BatchProjectResult{Results: []map[string]interface{}{{"line": i + 1, "project_id": p.ID, "error": "batch_partial_retry"}}}, domain.ErrConflict
		}
	}
	if allExisting {
		res := BatchProjectResult{Projects: existing, Fingerprint: fp}
		res.Results = make([]map[string]interface{}, len(existing))
		for i, p := range existing {
			res.Results[i] = map[string]interface{}{"line": i + 1, "id": p.ID, "project_id": p.ID, "revision": p.PlanRevision, "next": "/v1/projects/" + p.ID + "/baseline"}
		}
		s.idem[idem] = res
		return res, nil
	}
	ps := make([]*domain.RestorationProject, len(ns))
	now := s.now()
	for i, n := range ns {
		id := fmt.Sprintf("RP-%s-%d", now.Format("20060102"), now.UnixNano()%1000000+int64(i))
		p, e := domain.NewProject(id, n.a, n.t, n.c, now)
		if e != nil {
			return BatchProjectResult{}, e
		}
		p.CreateRequestID = n.r
		p.CreateFingerprint = n.fp
		ps[i] = p
	}
	if e := s.store.CreateBatch(ps); e != nil {
		s.event("", "project_batch_conflict", actor, idemKey, map[string]interface{}{"fingerprint": fp})
		return BatchProjectResult{}, e
	}
	res := BatchProjectResult{Projects: ps, Fingerprint: fp}
	res.Results = make([]map[string]interface{}, len(ps))
	for i, p := range ps {
		res.Results[i] = map[string]interface{}{"line": i + 1, "id": p.ID, "project_id": p.ID, "revision": p.PlanRevision, "next": "/v1/projects/" + p.ID + "/baseline"}
		s.event(p.ID, "project_created", actor, ns[i].r, map[string]interface{}{"batch_fingerprint": fp})
	}
	s.idem[idem] = res
	return res, nil
}
func (s *Service) CreatePreflight(asset, title, custodian, actor, request string) (ProjectPreflight, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ve error
	asset, title, custodian, ve = domain.ValidateProjectFields(asset, title, custodian)
	if ve != nil || request == "" {
		if ve != nil {
			return ProjectPreflight{}, ve
		}
		return ProjectPreflight{}, domain.ErrInvalid
	}
	fb, _ := json.Marshal(struct{ Asset, Title, Custodian string }{strings.ToUpper(asset), title, custodian})
	fp := fmt.Sprintf("%x", sha256.Sum256(fb))
	if r, ok := s.reservations[request]; ok && r.Fingerprint != fp && !r.Consumed && s.now().Before(r.ExpiresAt) {
		s.event("", "project_preflight_conflict", actor, request, map[string]interface{}{"reason": "request_id_reused", "fingerprint": fp})
		return ProjectPreflight{}, &domain.ProjectConflictError{Reason: "request_id 已用于不同文物信息"}
	}
	if r, ok := s.reservations[request]; ok && r.Fingerprint == fp && !r.Consumed && s.now().Before(r.ExpiresAt) {
		return ProjectPreflight{CanCreate: true, Fingerprint: fp, Next: "/v1/projects", ReservationToken: r.Token, CandidateID: r.Candidate, ExpiresAt: r.ExpiresAt}, nil
	}
	if p, e := s.store.FindByAsset(asset); e == nil {
		s.event(p.ID, "project_preflight_conflict", actor, request, map[string]interface{}{"asset_code": asset})
		return ProjectPreflight{CanCreate: false, ProjectID: p.ID, Status: p.Status, Revision: p.PlanRevision, Fingerprint: fp, RetryFingerprint: fp, Occupied: true, ConflictReason: "asset_code_duplicate", Next: "/v1/projects/" + p.ID + "/baseline"}, nil
	}
	token := fmt.Sprintf("PRT-%d", s.now().UnixNano())
	candidate := fmt.Sprintf("RP-%s-%d", s.now().Format("20060102"), s.now().UnixNano()%1000000)
	exp := s.now().Add(10 * time.Minute)
	s.reservations[request] = reservation{Token: token, Asset: strings.ToUpper(asset), Fingerprint: fp, Candidate: candidate, ExpiresAt: exp}
	s.event("", "project_preflight", actor, request, map[string]interface{}{"asset_code": asset, "fingerprint": fp, "reservation_token": token, "expires_at": exp})
	return ProjectPreflight{CanCreate: true, Fingerprint: fp, Next: "/v1/projects", ReservationToken: token, CandidateID: candidate, ExpiresAt: exp}, nil
}
func (s *Service) Baseline(id, plan string, materials interface{}, risk, actor string, expected int, request string, confirmation ...string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	cleanMaterials, diag, normErr := domain.NormalizeMaterials(materials)
	if normErr != nil {
		fields := map[string]string{}
		for _, d := range diag {
			if d["field"] != "" {
				fields[d["field"]] = d["reason"]
			}
		}
		if len(fields) > 0 {
			return nil, &domain.FieldValidationError{Fields: fields}
		}
		return nil, domain.ErrInvalid
	}
	cleanMaterials = canonicalMaterials(cleanMaterials)
	fb, _ := json.Marshal(struct {
		Plan      string
		Materials []domain.MaterialEntry
		Risk      string
	}{strings.Join(strings.Fields(plan), " "), cleanMaterials, strings.TrimSpace(risk)})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fb))
	needsApproval := p.RiskLevel != "" && domainRiskRank(risk) > domainRiskRank(p.RiskLevel)
	preChanges, _, _ := p.BaselineDiff(plan, cleanMaterials, risk)
	// Material reductions on high-risk projects require an impact explanation and approver.
	if p.RiskLevel == "high" {
		for _, ch := range preChanges {
			if ch["type"] == "modified" && ch["field"] == "materials" {
				if b, ok := ch["before"].(domain.MaterialEntry); ok {
					if a, ok := ch["after"].(domain.MaterialEntry); ok && b.Quantity > 0 && a.Quantity < b.Quantity*0.8 {
						needsApproval = true
					}
				}
			}
		}
	}
	for _, pr := range p.Procedures {
		if pr.StartedAt != nil {
			needsApproval = true
			break
		}
	}
	if needsApproval && (len(confirmation) < 3 || confirmation[0] == "" || strings.TrimSpace(confirmation[1]) == "" || strings.TrimSpace(confirmation[2]) == "") {
		return nil, domain.ErrConflict
	}
	if len(confirmation) > 0 && confirmation[0] != "" {
		if confirmation[0] != fingerprint {
			return nil, domain.ErrConflict
		}
	}
	if request != "" && p.BaselineRequestID == request {
		if p.BaselineFingerprint == fingerprint {
			s.event(id, "baseline_idempotent", actor, request, map[string]interface{}{"plan_revision": p.PlanRevision})
			return p, nil
		}
		s.event(id, "baseline_conflict", actor, request, map[string]interface{}{"reason": "request_id_reused"})
		return nil, domain.ErrConflict
	}
	if expected > 0 && p.PlanRevision != expected {
		s.event(id, "baseline_conflict", actor, request, map[string]interface{}{"current_revision": p.PlanRevision})
		return nil, domain.ErrConflict
	}
	if e = p.SetBaseline(plan, cleanMaterials, risk, s.now()); e != nil {
		return nil, e
	}
	p.BaselineRequestID, p.BaselineFingerprint = request, fingerprint
	br := domain.BaselineRevision{Revision: p.PlanRevision, Plan: p.Plan, Materials: append([]domain.MaterialEntry(nil), p.Materials...), RiskLevel: p.RiskLevel, Operator: actor, At: s.now(), Changes: preChanges}
	if len(confirmation) > 1 {
		br.Approver = confirmation[1]
	}
	if len(confirmation) > 2 {
		br.ImpactReason = confirmation[2]
	}
	p.BaselineHistory = append(p.BaselineHistory, br)
	if e = s.store.Update(p, expected); e != nil {
		return nil, e
	}
	data := map[string]interface{}{"risk": risk, "plan_revision": p.PlanRevision}
	if needsApproval {
		data["approver"], data["approval_reason"] = confirmation[1], confirmation[2]
	}
	s.event(id, "baseline_locked", actor, request, data)
	return p, nil
}

func domainRiskRank(r string) int {
	switch strings.TrimSpace(r) {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	}
	return 0
}

func canonicalMaterials(ms []domain.MaterialEntry) []domain.MaterialEntry {
	out := append([]domain.MaterialEntry(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool {
		ki := strings.ToLower(strings.TrimSpace(out[i].Name)) + "|" + strings.ToLower(strings.TrimSpace(out[i].Batch))
		kj := strings.ToLower(strings.TrimSpace(out[j].Name)) + "|" + strings.ToLower(strings.TrimSpace(out[j].Batch))
		return ki < kj
	})
	return out
}

func (s *Service) BaselinePreflight(id, plan string, materials interface{}, risk, actor, request string, expected int) (domain.BaselinePreflight, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return domain.BaselinePreflight{}, e
	}
	if expected > 0 && p.PlanRevision != expected {
		return domain.BaselinePreflight{}, domain.ErrConflict
	}
	missing := p.BaselineCheck(plan, materials, risk)
	changes, upgrade, diffMissing := p.BaselineDiff(plan, materials, risk)
	if len(diffMissing) > 0 {
		missing = append(missing, diffMissing...)
	}
	norm, _, _ := domain.NormalizeMaterials(materials)
	norm = canonicalMaterials(norm)
	fb, _ := json.Marshal(struct {
		Plan      string
		Materials []domain.MaterialEntry
		Risk      string
	}{strings.Join(strings.Fields(plan), " "), norm, strings.TrimSpace(risk)})
	fp := fmt.Sprintf("%x", sha256.Sum256(fb))
	s.event(id, "baseline_preflight", actor, request, map[string]interface{}{"missing": missing, "fingerprint": fp, "plan_revision": p.PlanRevision})
	return domain.BaselinePreflight{Missing: missing, Fingerprint: fp, Revision: p.PlanRevision, Changes: changes, RiskUpgrade: upgrade}, nil
}
func (s *Service) AddProcedure(id, name, tech string, seq int, actor string) (*domain.ProcedureRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	pr := &domain.ProcedureRecord{ID: fmt.Sprintf("PR-%d", s.now().UnixNano()), Name: name, Technician: tech, Sequence: seq}
	for _, old := range p.Procedures {
		if old.ID == pr.ID {
			pr.ID = fmt.Sprintf("PR-%d-x", s.now().UnixNano())
			break
		}
	}
	if e = p.AddProcedure(pr, s.now()); e != nil {
		return nil, e
	}
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "procedure_created", actor, "", map[string]interface{}{"procedure_id": pr.ID})
	return pr, nil
}
func (s *Service) AddProcedures(id string, items []*domain.ProcedureRecord, actor string) ([]*domain.ProcedureRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	ids := map[string]bool{}
	for _, old := range p.Procedures {
		ids[old.ID] = true
	}
	for i, pr := range items {
		if pr == nil {
			return nil, domain.ErrInvalid
		}
		if pr.ID == "" || ids[pr.ID] {
			seed := fmt.Sprintf("%s:%d:%s", id, pr.Sequence, strings.TrimSpace(pr.Name))
			pr.ID = "PR-" + fmt.Sprintf("%x", sha256.Sum256([]byte(seed)))[:12]
			if ids[pr.ID] {
				pr.ID = fmt.Sprintf("PR-%d-%d", s.now().UnixNano(), i)
			}
		}
		ids[pr.ID] = true
	}
	if e = p.AddProcedures(items, s.now()); e != nil {
		return nil, e
	}
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "procedure_created", actor, "", map[string]interface{}{"count": len(items)})
	return p.Procedures, nil
}

func (s *Service) AddProceduresBatch(id string, items []*domain.ProcedureRecord, actor, request string, validateOnly bool) (ProcedureBatchResult, error) {
	return s.AddProceduresBatchLimit(id, items, actor, request, validateOnly, 3)
}
func (s *Service) AddProceduresBatchLimit(id string, items []*domain.ProcedureRecord, actor, request string, validateOnly bool, workloadLimit int, guards ...interface{}) (ProcedureBatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return ProcedureBatchResult{}, e
	}
	if len(guards) > 0 {
		if rev, ok := guards[0].(int); ok && rev > 0 && p.PlanRevision != rev {
			return ProcedureBatchResult{}, domain.ErrConflict
		}
	}
	// Normalize deterministic IDs before validation so retries can be resolved idempotently.
	ids0 := map[string]bool{}
	for _, old := range p.Procedures {
		ids0[old.ID] = true
	}
	for i, pr := range items {
		if pr == nil {
			return ProcedureBatchResult{}, domain.ErrInvalid
		}
		if pr.ID == "" {
			seed := fmt.Sprintf("%s:%d:%s", id, pr.Sequence, strings.TrimSpace(pr.Name))
			pr.ID = "PR-" + fmt.Sprintf("%x", sha256.Sum256([]byte(seed)))[:12]
		}
		if ids0[pr.ID] && pr.ID != "" { /* existing IDs are validated by the domain */
		}
		ids0[pr.ID] = true
		_ = i
	}
	fb0, _ := json.Marshal(items)
	fp0 := fmt.Sprintf("%x", sha256.Sum256(fb0))
	if request != "" {
		if p.ProcedureRequestID == request {
			if p.ProcedureFingerprint != fp0 {
				return ProcedureBatchResult{}, domain.ErrConflict
			}
			return ProcedureBatchResult{Procedures: p.Procedures, Fingerprint: fp0, Valid: true, Revision: p.PlanRevision}, nil
		}
		if v, ok := s.idem["proc:"+request]; ok {
			if old, ok := v.(ProcedureBatchResult); ok {
				if old.Fingerprint != fp0 {
					return ProcedureBatchResult{}, domain.ErrConflict
				}
				return old, nil
			}
		}
	}
	var cp domain.RestorationProject
	if raw, marshalErr := json.Marshal(p); marshalErr == nil {
		if unmarshalErr := json.Unmarshal(raw, &cp); unmarshalErr != nil {
			return ProcedureBatchResult{}, unmarshalErr
		}
	} else {
		return ProcedureBatchResult{}, marshalErr
	}
	ids := map[string]bool{}
	for _, old := range p.Procedures {
		ids[old.ID] = true
	}
	for i, pr := range items {
		if pr == nil {
			return ProcedureBatchResult{}, domain.ErrInvalid
		}
		if pr.ID == "" || ids[pr.ID] {
			pr.ID = fmt.Sprintf("PR-%d-%d", s.now().UnixNano(), i)
		}
		ids[pr.ID] = true
	}
	fp := fp0
	if len(guards) > 1 {
		if expectedFP, ok := guards[1].(string); ok && expectedFP != "" && expectedFP != fp {
			return ProcedureBatchResult{}, domain.ErrConflict
		}
	}
	warnings := []string{}
	techCount := map[string]int{}
	for _, old := range p.Procedures {
		if !old.Completed {
			techCount[strings.TrimSpace(old.Technician)]++
		}
	}
	for _, pr := range items {
		techCount[strings.TrimSpace(pr.Technician)]++
	}
	if workloadLimit < 1 {
		workloadLimit = 3
	}
	for t, n := range techCount {
		if n > workloadLimit {
			if validateOnly {
				return ProcedureBatchResult{Procedures: items, Errors: []map[string]string{{"field": "technician", "technician": t, "reason": fmt.Sprintf("容量超限：%d>%d", n, workloadLimit)}}, Fingerprint: fp, Valid: false, Revision: p.PlanRevision}, nil
			}
			return ProcedureBatchResult{Fingerprint: fp, Valid: false, Revision: p.PlanRevision}, &domain.CapacityError{Technician: t, Limit: workloadLimit, Count: n}
		}
	}
	// Expose independent diagnostics for validate_only callers so a malformed
	// batch can be corrected in one round trip.
	if validateOnly {
		diags := []map[string]string{}
		seenNames := map[string]int{}
		seenSeq := map[int]bool{}
		for i, pr := range items {
			if pr.Sequence != i+1 {
				diags = append(diags, map[string]string{"field": fmt.Sprintf("procedures[%d].sequence", i), "reason": "序号必须连续"})
			}
			key := strings.ToLower(strings.TrimSpace(pr.Name))
			if j, ok := seenNames[key]; ok {
				diags = append(diags, map[string]string{"field": fmt.Sprintf("procedures[%d].name", i), "reason": fmt.Sprintf("名称与 procedures[%d] 重复", j)})
			}
			seenNames[key] = i
			if seenSeq[pr.Sequence] {
				diags = append(diags, map[string]string{"field": fmt.Sprintf("procedures[%d].sequence", i), "reason": "序号重复"})
			}
			seenSeq[pr.Sequence] = true
		}
		if len(diags) > 0 {
			s.event(id, "procedure_batch_preflight", actor, request, map[string]interface{}{"errors": diags, "fingerprint": fp})
			return ProcedureBatchResult{Procedures: items, Errors: diags, Fingerprint: fp, Valid: false, Revision: p.PlanRevision}, nil
		}
	}
	err := cp.AddProcedures(items, s.now())
	if err != nil {
		if validateOnly {
			diags := []map[string]string{{"field": "procedures", "reason": err.Error()}}
			return ProcedureBatchResult{Procedures: items, Warnings: warnings, Errors: diags, Fingerprint: fp, Valid: false, Revision: p.PlanRevision}, nil
		}
		return ProcedureBatchResult{Warnings: warnings, Fingerprint: fp, Valid: false}, err
	}
	if validateOnly {
		s.event(id, "procedure_batch_preflight", actor, request, map[string]interface{}{"count": len(items), "warnings": warnings, "fingerprint": fp})
		return ProcedureBatchResult{Procedures: items, Warnings: warnings, Fingerprint: fp, Valid: true, Revision: p.PlanRevision}, nil
	}
	if e = p.AddProcedures(items, s.now()); e != nil {
		return ProcedureBatchResult{}, e
	}
	p.ProcedureRequestID, p.ProcedureFingerprint = request, fp
	if e = s.store.Update(p, 0); e != nil {
		return ProcedureBatchResult{}, e
	}
	out := ProcedureBatchResult{Procedures: items, Warnings: warnings, Fingerprint: fp, Valid: true, Revision: p.PlanRevision}
	if request != "" {
		s.idem["proc:"+request] = out
	}
	s.event(id, "procedure_batch_created", actor, request, map[string]interface{}{"count": len(items), "warnings": warnings})
	return out, nil
}

func (s *Service) VerifyEvidence(id, actor, request string) (domain.EvidenceVerification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return domain.EvidenceVerification{}, e
	}
	v := p.VerifyEvidence()
	s.event(id, "evidence_verification", actor, request, map[string]interface{}{"evidence_count": v.EvidenceCount, "issues": len(v.Issues), "summary_hash": v.SummaryHash})
	return v, nil
}

func (s *Service) ListEvidence(id, procedureID, kind string, since, until *time.Time, pageSize int, cursor string, actor, request string) (map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if kind != "" && kind != "photo" && kind != "document" && kind != "measurement" && kind != "instrument" && kind != "report" {
		return nil, domain.ErrInvalid
	}
	all := make([]*domain.EvidenceItem, 0)
	for _, ev := range p.Evidence {
		if ev.Superseded {
			continue
		}
		if procedureID != "" && ev.ProcedureID != procedureID || kind != "" && strings.ToLower(ev.Kind) != kind || since != nil && ev.CapturedAt.Before(*since) || until != nil && ev.CapturedAt.After(*until) {
			continue
		}
		all = append(all, ev)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CapturedAt.Equal(all[j].CapturedAt) {
			return all[i].ID < all[j].ID
		}
		return all[i].CapturedAt.Before(all[j].CapturedAt)
	})
	start := 0
	snapshotRevision := p.PlanRevision
	if cursor != "" {
		if strings.Contains(cursor, ":") {
			var rev int
			var cid string
			if _, e := fmt.Sscanf(cursor, "%d:%s", &rev, &cid); e == nil {
				if rev != snapshotRevision {
					return nil, fmt.Errorf("stale_snapshot")
				}
				cursor = cid
			}
		}
		found := false
		for i, v := range all {
			if v.ID == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, domain.ErrInvalid
		}
	}
	if pageSize < 1 || pageSize > 200 {
		return nil, domain.ErrInvalid
	}
	total := len(all)
	end := start + pageSize
	if end > total {
		end = total
	}
	out := all[start:end]
	next := ""
	if end < total {
		next = fmt.Sprintf("%d:%s", snapshotRevision, out[len(out)-1].ID)
	}
	views := make([]map[string]interface{}, 0, len(out))
	completed := map[string]bool{}
	for _, pr := range p.Procedures {
		completed[pr.ID] = pr.Completed
	}
	for _, ev := range out {
		views = append(views, map[string]interface{}{"id": ev.ID, "project_id": ev.ProjectID, "procedure_id": ev.ProcedureID, "kind": ev.Kind, "uri": ev.URI, "sha256": ev.SHA256, "captured_at": ev.CapturedAt, "hash_status": "valid", "procedure_completed": completed[ev.ProcedureID]})
	}
	v := p.VerifyEvidence()
	s.event(id, "evidence_index_query", actor, request, map[string]interface{}{"kind": kind, "procedure_id": procedureID, "total": total})
	return map[string]interface{}{"evidence": views, "total": total, "next_cursor": next, "snapshot_revision": snapshotRevision, "verification": v}, nil
}
func (s *Service) EvidenceCoverage(id, kind string) (map[string]interface{}, error) {
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	types := []string{"photo", "measurement", "report"}
	if p.RiskLevel == "low" {
		types = []string{"photo"}
	}
	gaps := []map[string]interface{}{}
	kindCounts := map[string]int{}
	procedureCounts := map[string]int{}
	details := make([]*domain.EvidenceItem, 0)
	for _, ev := range p.Evidence {
		if ev.Superseded {
			continue
		}
		if kind != "" && strings.ToLower(ev.Kind) != strings.ToLower(kind) {
			continue
		}
		kindCounts[strings.ToLower(ev.Kind)]++
		procedureCounts[ev.ProcedureID]++
		details = append(details, ev)
	}
	complete := true
	for _, pr := range p.Procedures {
		have := map[string]int{}
		for _, ev := range p.Evidence {
			if ev.Superseded {
				continue
			}
			if ev.ProcedureID == pr.ID {
				have[strings.ToLower(ev.Kind)]++
			}
		}
		missing := []string{}
		for _, k := range types {
			if kind != "" && k != kind {
				continue
			}
			if have[k] == 0 {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 || !pr.Completed {
			complete = false
			gaps = append(gaps, map[string]interface{}{"procedure_id": pr.ID, "covered_types": have, "missing_types": missing, "evidence_count": len(pr.EvidenceIDs), "completed": pr.Completed})
		}
	}
	return map[string]interface{}{"coverage_complete": complete, "gaps": gaps, "summary_hash": p.VerifyEvidence().SummaryHash, "kind_counts": kindCounts, "procedure_counts": procedureCounts, "evidence": details, "total": len(details)}, nil
}
func (s *Service) CompleteProcedure(id, pid string, start, end time.Time, env, instruction, result, actor, request string, evidence []*domain.EvidenceItem, expected ...int) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type evSig struct {
		Kind, URI, SHA256 string
		CapturedAt        time.Time
		Metadata          map[string]string
	}
	evnorm := make([]evSig, 0, len(evidence))
	for _, e := range evidence {
		evnorm = append(evnorm, evSig{e.Kind, e.URI, e.SHA256, e.CapturedAt, e.Metadata})
	}
	sigBytes, _ := json.Marshal(struct {
		ID, PID, Env, Instruction, Result string
		Start, End                        time.Time
		Evidence                          []evSig
	}{id, pid, env, instruction, result, start, end, evnorm})
	sig := fmt.Sprintf("%x", sha256.Sum256(sigBytes))
	if request != "" {
		if v, ok := s.idem[request]; ok {
			if rec, ok := v.(struct {
				sig string
				p   *domain.RestorationProject
			}); ok {
				if rec.sig != sig {
					return nil, domain.ErrConflict
				}
				return rec.p, nil
			}
		}
	}
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if len(expected) > 0 && expected[0] > 0 && p.PlanRevision != expected[0] {
		return nil, domain.ErrConflict
	}
	if e = p.CompleteProcedure(pid, start, end, env, instruction, result, evidence, s.now()); e != nil {
		if strings.HasPrefix(e.Error(), "environment_abnormal") {
			s.event(id, "environment_abnormal", actor, request, map[string]interface{}{"procedure_id": pid, "reason": e.Error()})
		}
		return nil, e
	}
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "procedure_completed", actor, request, map[string]interface{}{"procedure_id": pid, "evidence_count": len(evidence)})
	for _, pr := range p.Procedures {
		if pr.ID == pid && pr.TrendStatus == "warning" {
			s.event(id, "environment_trend_warning", actor, request, map[string]interface{}{"procedure_id": pid, "warnings": pr.TrendWarnings})
		}
	}
	if request != "" {
		s.idem[request] = struct {
			sig string
			p   *domain.RestorationProject
		}{sig, p}
	}
	return p, nil
}

func (s *Service) ReopenProcedure(id, pid, inspectionID, reason, actor, request string, expected int) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if expected > 0 && p.PlanRevision != expected {
		return nil, domain.ErrConflict
	}
	if e = p.ReopenProcedure(pid, inspectionID, reason, s.now()); e != nil {
		return nil, e
	}
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "procedure_reopened", actor, request, map[string]interface{}{"procedure_id": pid, "inspection_id": inspectionID, "reason": reason})
	return p, nil
}
func (s *Service) Inspect(id, inspector, decision string, findings []string, due *time.Time, actor, request string, sampled []string, evidenceIDs []string, expected ...int) (*domain.InspectionBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if len(expected) > 0 && expected[0] > 0 && p.PlanRevision != expected[0] {
		return nil, domain.ErrConflict
	}
	normFindings := append([]string(nil), findings...)
	for i := range normFindings {
		normFindings[i] = strings.TrimSpace(normFindings[i])
	}
	sort.Strings(normFindings)
	fb, _ := json.Marshal(struct {
		Inspector, Decision string
		Findings            []string
		Due                 *time.Time
	}{strings.TrimSpace(inspector), strings.TrimSpace(decision), normFindings, due})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fb))
	if request != "" {
		for _, old := range p.Inspections {
			if old.RequestID == request {
				if old.RequestFingerprint == fingerprint {
					return old, nil
				}
				return nil, domain.ErrConflict
			}
		}
	}
	i := &domain.InspectionBatch{ID: fmt.Sprintf("IN-%d", s.now().UnixNano()), Inspector: inspector, Decision: decision, Findings: findings, DueAt: due}
	i.SampledProcedureIDs = append([]string(nil), sampled...)
	i.EvidenceIDs = append([]string(nil), evidenceIDs...)
	i.RequestID, i.RequestFingerprint = request, fingerprint
	if e = p.AddInspection(i, s.now()); e != nil {
		return nil, e
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "inspection_created", actor, "", map[string]interface{}{"inspection_id": i.ID, "decision": decision})
	return i, nil
}
func (s *Service) ReviseInspection(id, inspectionID string, expected int, inspector, decision string, findings []string, due *time.Time, sampled, evidenceIDs []string, actor, request string) (*domain.InspectionBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	var old *domain.InspectionBatch
	for _, i := range p.Inspections {
		if i.ID == inspectionID {
			old = i
			break
		}
	}
	if old == nil {
		return nil, domain.ErrNotFound
	}
	if old.Frozen {
		return nil, domain.ErrConflict
	}
	if expected > 0 && p.PlanRevision != expected {
		return nil, domain.ErrConflict
	}
	fb, _ := json.Marshal(struct {
		I, D string
		F    []string
		E, S []string
	}{inspector, decision, findings, evidenceIDs, sampled})
	fp := fmt.Sprintf("%x", sha256.Sum256(fb))
	if request != "" && old.RequestID == request {
		if old.RequestFingerprint == fp {
			return old, nil
		}
		return nil, domain.ErrConflict
	}
	cleanFindings := make([]string, 0, len(findings))
	for _, f := range findings {
		if f = strings.TrimSpace(f); f != "" {
			cleanFindings = append(cleanFindings, f)
		}
	}
	findings = cleanFindings
	if (decision == "pass" && (len(findings) > 0 || due != nil)) || (decision == "remediate" && (len(findings) == 0 || due == nil)) || (decision == "fail" && (len(findings) == 0 || due != nil)) {
		return nil, domain.ErrInvalid
	}
	if decision == "remediate" && due != nil {
		window := 14 * 24 * time.Hour
		if p.RiskLevel == "medium" {
			window = 7 * 24 * time.Hour
		}
		if p.RiskLevel == "high" {
			window = 3 * 24 * time.Hour
		}
		if due.Before(s.now()) || due.After(s.now().Add(window)) {
			return nil, fmt.Errorf("due_at_out_of_window")
		}
	}
	if decision != "pass" && decision != "remediate" && decision != "fail" {
		return nil, domain.ErrInvalid
	}
	for _, pid := range sampled {
		found := false
		for _, pr := range p.Procedures {
			if pr.ID == pid {
				found = true
				break
			}
		}
		if !found {
			return nil, domain.ErrConflict
		}
	}
	for _, eid := range evidenceIDs {
		found := false
		for _, ev := range p.Evidence {
			if ev.ID == eid {
				found = true
				break
			}
		}
		if !found {
			return nil, domain.ErrConflict
		}
	}
	if decision == "pass" {
		for _, r := range p.Remediations {
			if r.Status != "closed" {
				r.Status = "closed"
			}
		}
		p.Status = domain.StatusInspection
	} else {
		p.Status = domain.StatusRemediation
	}
	old.Revision++
	old.RevisionHistory = append(old.RevisionHistory, domain.InspectionRevision{Revision: old.Revision, Decision: old.Decision, Inspector: old.Inspector, Findings: append([]string(nil), old.Findings...), At: s.now()})
	old.Decision, old.Inspector, old.Findings, old.DueAt = decision, inspector, findings, due
	old.SampledProcedureIDs = sampled
	old.EvidenceIDs = evidenceIDs
	old.RequestID, old.RequestFingerprint = request, fp
	p.PlanRevision++
	p.UpdatedAt = s.now()
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "inspection_revised", actor, request, map[string]interface{}{"inspection_id": inspectionID, "revision": old.Revision})
	return old, nil
}
func (s *Service) Remediate(id, inspection, description, assignee string, due *time.Time, actor string) (*domain.Remediation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	r := &domain.Remediation{ID: fmt.Sprintf("RM-%d", s.now().UnixNano()), InspectionID: inspection, Description: description, Assignee: assignee, DueAt: due}
	if e = p.AddRemediation(r, s.now()); e != nil {
		return nil, e
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "remediation_created", actor, "", map[string]interface{}{"remediation_id": r.ID})
	return r, nil
}
func (s *Service) Resolve(id, rid string, evidence []string, reviewer, actor string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if e = p.ResolveRemediation(rid, evidence, reviewer, s.now()); e != nil {
		return nil, e
	}
	open := false
	for _, r := range p.Remediations {
		if r.Status != "closed" {
			open = true
		}
	}
	if !open {
		p.Status = domain.StatusInspection
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "remediation_resolved", actor, "", map[string]interface{}{"remediation_id": rid, "reviewer": reviewer})
	return p, nil
}
func (s *Service) ResolveDecision(id, rid, decision, reason string, evidence []string, reviewer, actor, request string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sigBytes, _ := json.Marshal(struct {
		D, R string
		E    []string
	}{decision, reason, evidence})
	sig := fmt.Sprintf("%x", sha256.Sum256(sigBytes))
	if request != "" {
		if v, ok := s.idem["resolve:"+request]; ok {
			if old, ok := v.(struct {
				sig string
				p   *domain.RestorationProject
			}); ok {
				if old.sig != sig {
					return nil, domain.ErrConflict
				}
				return old.p, nil
			}
		}
	}
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	for _, r := range p.Remediations {
		if r.ID == rid && strings.TrimSpace(r.Assignee) == strings.TrimSpace(reviewer) {
			return nil, domain.ErrInvalid
		}
	}
	if decision == "reject" {
		if strings.TrimSpace(reason) == "" {
			return nil, domain.ErrInvalid
		}
		for _, r := range p.Remediations {
			if r.ID == rid {
				r.Status = "open"
				r.ReviewedBy = reviewer
				r.EvidenceIDs = nil
				r.ReviewReason = reason
				p.PlanRevision++
				if e := s.store.Update(p, 0); e != nil {
					return nil, e
				}
				if request != "" {
					s.idem["resolve:"+request] = struct {
						sig string
						p   *domain.RestorationProject
					}{sig, p}
				}
				s.event(id, "remediation_rejected", actor, request, map[string]interface{}{"remediation_id": rid, "reason": reason})
				return p, nil
			}
		}
		return nil, domain.ErrNotFound
	}
	if decision != "approve" {
		return nil, domain.ErrInvalid
	}
	if e = p.ResolveRemediation(rid, evidence, reviewer, s.now()); e != nil {
		return nil, e
	}
	open := false
	for _, r := range p.Remediations {
		if r.Status != "closed" {
			open = true
		}
	}
	if !open {
		p.Status = domain.StatusInspection
	}
	p.PlanRevision++
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "remediation_approved", actor, request, map[string]interface{}{"remediation_id": rid})
	if request != "" {
		s.idem["resolve:"+request] = struct {
			sig string
			p   *domain.RestorationProject
		}{sig, p}
	}
	return p, nil
}

func (s *Service) ListRemediations(id, assignee, status string, since, until *time.Time, page, size int) ([]*domain.Remediation, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, 0, nil, e
	}
	now := s.now()
	all := []*domain.Remediation{}
	stats := map[string]int{"open": 0, "closed": 0, "overdue": 0}
	changed := false
	for _, r := range p.Remediations {
		if r.Status == "open" && r.DueAt != nil && r.DueAt.Before(now) {
			r.Status = "overdue"
			if r.EscalatedAt == nil {
				t := now
				r.EscalatedAt = &t
				s.event(id, "remediation_overdue", "system", "", map[string]interface{}{"remediation_id": r.ID})
				changed = true
			}
		}
		if assignee != "" && r.Assignee != assignee {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		if since != nil && r.DueAt != nil && r.DueAt.Before(*since) {
			continue
		}
		if until != nil && r.DueAt != nil && r.DueAt.After(*until) {
			continue
		}
		all = append(all, r)
		if r.Status == "overdue" {
			stats["overdue"]++
		} else {
			stats[r.Status]++
		}
	}
	if changed {
		_ = s.store.Update(p, 0)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	total := len(all)
	if page < 1 || size < 1 {
		return nil, total, stats, domain.ErrInvalid
	}
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return all[start:end], total, stats, nil
}

func (s *Service) ResolveBatch(id string, updates []struct {
	ID       string
	Evidence []string
}, reviewer, actor string) (*domain.RestorationProject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	cp := *p
	b, _ := json.Marshal(p)
	_ = json.Unmarshal(b, &cp)
	seen := map[string]bool{}
	for _, u := range updates {
		if seen[u.ID] {
			return nil, domain.ErrInvalid
		}
		seen[u.ID] = true
		if e := cp.ResolveRemediation(u.ID, u.Evidence, reviewer, s.now()); e != nil {
			return nil, e
		}
	}
	open := false
	for _, r := range cp.Remediations {
		if r.Status != "closed" {
			open = true
		}
	}
	if !open {
		cp.Status = domain.StatusInspection
	}
	cp.PlanRevision++
	if e = s.store.Update(&cp, 0); e != nil {
		return nil, e
	}
	for _, u := range updates {
		s.event(id, "remediation_resolved", actor, "", map[string]interface{}{"remediation_id": u.ID, "reviewer": reviewer})
	}
	return &cp, nil
}
func (s *Service) Release(id string, reviewers []string, opinions map[string]string, actor string) (*domain.ReleaseArchive, error) {
	return s.ReleaseWithRequest(id, reviewers, opinions, actor, "")
}
func countApproved(op map[string]string) int {
	n := 0
	for _, v := range op {
		if strings.ToLower(strings.TrimSpace(v)) == "approve" || v == "同意" || v == "通过" {
			n++
		}
	}
	return n
}

func (s *Service) ReleasePreflight(id, actor, request string) (map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if !s.VerifyArchives(p) {
		s.event(id, "archive_integrity_blocked", actor, request, map[string]interface{}{"reason": "归档校验失败"})
		return map[string]interface{}{"valid": false, "missing": []map[string]string{{"type": "integrity_error", "reason": "归档校验失败"}}, "revision": p.PlanRevision}, nil
	}
	missing := []map[string]string{}
	if len(p.Evidence) == 0 {
		missing = append(missing, map[string]string{"type": "evidence", "reason": "缺少证据索引"})
	}
	for _, pr := range p.Procedures {
		if !pr.Completed {
			missing = append(missing, map[string]string{"type": "procedure", "id": pr.ID, "reason": "工序未完成"})
		}
	}
	v := p.VerifyEvidence()
	for _, i := range v.Issues {
		missing = append(missing, map[string]string{"type": i.Type, "id": i.EvidenceID, "reason": i.Detail})
	}
	for _, r := range p.Remediations {
		if r.Status != "closed" {
			missing = append(missing, map[string]string{"type": "remediation", "id": r.ID, "reason": "整改未关闭"})
		}
	}
	if len(p.Inspections) == 0 {
		missing = append(missing, map[string]string{"type": "inspection", "reason": "缺少质检批次"})
	} else {
		frozen := false
		for _, in := range p.Inspections {
			if in.Frozen {
				frozen = true
				break
			}
		}
		if !frozen {
			missing = append(missing, map[string]string{"type": "inspection", "reason": "质检批次尚未冻结"})
		}
	}
	if !s.audit.Verify(id) {
		missing = append(missing, map[string]string{"type": "audit", "reason": "审计哈希链不连续"})
	}
	ev := s.audit.List(id)
	tail := ""
	if len(ev) > 0 {
		tail = ev[len(ev)-1].Hash
	}
	raw, _ := json.Marshal(struct {
		Rev        int
		Root, Tail string
	}{p.PlanRevision, v.SummaryHash, tail})
	fp := fmt.Sprintf("%x", sha256.Sum256(raw))
	s.event(id, "release_preflight", actor, request, map[string]interface{}{"missing": len(missing), "fingerprint": fp})
	s.preflight[id] = fp
	return map[string]interface{}{"valid": len(missing) == 0, "missing": missing, "fingerprint": fp, "revision": p.PlanRevision, "evidence_root": v.SummaryHash}, nil
}

func (s *Service) ReleaseWithReport(id string, reviewers []string, opinions map[string]string, actor, request, report string) (*domain.ReleaseArchive, error) {
	if report == "" {
		return s.ReleaseWithRequest(id, reviewers, opinions, actor, request)
	}
	s.mu.Lock()
	want := s.preflight[id]
	ev := s.audit.List(id)
	valid := len(ev) > 0 && ev[len(ev)-1].Action == "release_preflight"
	s.mu.Unlock()
	if !valid || report != want {
		return nil, domain.ErrConflict
	}
	return s.ReleaseWithRequest(id, reviewers, opinions, actor, request)
}
func (s *Service) ReleaseWithRequest(id string, reviewers []string, opinions map[string]string, actor, request string) (*domain.ReleaseArchive, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reviewers = append([]string(nil), reviewers...)
	for i := range reviewers {
		reviewers[i] = strings.TrimSpace(reviewers[i])
	}
	seenReviewers := map[string]bool{}
	for _, r := range reviewers {
		if r == "" || seenReviewers[strings.ToLower(r)] {
			return nil, domain.ErrConflict
		}
		seenReviewers[strings.ToLower(r)] = true
	}
	sort.Strings(reviewers)
	fb, _ := json.Marshal(struct {
		ID        string
		Reviewers []string
		Opinions  map[string]string
	}{id, reviewers, opinions})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fb))
	if request != "" {
		if v, ok := s.idem["release:"+request]; ok {
			if a, ok := v.(*domain.ReleaseArchive); ok {
				if a.RequestFingerprint != fingerprint {
					return nil, domain.ErrConflict
				}
				return a, nil
			}
		}
	}
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	if len(p.Inspections) == 0 {
		return nil, domain.ErrConflict
	}
	for _, in := range p.Inspections {
		if !in.Frozen {
			return nil, domain.ErrConflict
		}
	}
	if request != "" {
		for _, ar := range p.Archives {
			if ar.RequestID == request {
				if ar.RequestFingerprint != fingerprint {
					return nil, domain.ErrConflict
				}
				return ar, nil
			}
		}
	}
	required := 2
	if p.RiskLevel == "high" {
		required = 3
	}
	if p.RiskLevel == "low" {
		required = 2
	}
	if len(reviewers) < required {
		return nil, domain.ErrConflict
	}
	approved := 0
	for _, reviewer := range reviewers {
		op, ok := opinions[reviewer]
		if !ok {
			// tolerate case-insensitive keys while still requiring every reviewer opinion
			for k, v := range opinions {
				if strings.EqualFold(k, reviewer) {
					op, ok = v, true
					break
				}
			}
		}
		if !ok || strings.TrimSpace(op) == "" {
			return nil, domain.ErrInvalid
		}
		switch strings.ToLower(strings.TrimSpace(op)) {
		case "approve", "同意", "通过":
			approved++
		case "reject", "abstain", "拒绝", "弃权":
		default:
			return nil, domain.ErrInvalid
		}
	}
	if approved < required {
		// Preserve dissent/abstention as an immutable round for later re-signing.
		round := domain.ReleaseRound{Round: len(p.ReleaseRounds) + 1, RequestID: request, Fingerprint: fingerprint, Reviewers: append([]string(nil), reviewers...), Opinions: opinions, At: s.now()}
		if v := p.VerifyEvidence(); v.Current {
			round.EvidenceRoot = v.SummaryHash
		}
		p.ReleaseRounds = append(p.ReleaseRounds, round)
		// A failed round is normally a quorum conflict, but persistence errors
		// must not escape as a second, indistinguishable conflict.  The current
		// implementation records the attempted write and deliberately maps the
		// storage failure to the same domain error, losing the resource cause.
		if updateErr := s.store.Update(p, 0); updateErr != nil {
			s.event(id, "release_round_persistence_failed", actor, request, map[string]interface{}{"round": round.Round, "error": updateErr.Error()})
			return nil, domain.ErrConflict
		}
		s.event(id, "release_round_failed", actor, request, map[string]interface{}{"round": round.Round, "fingerprint": fingerprint, "approved": approved, "required": required})
		return nil, domain.ErrConflict
	}
	if verification := p.VerifyEvidence(); !verification.Current {
		return nil, domain.ErrConflict
	}
	a, e := p.Release(fmt.Sprintf("AR-%d", s.now().UnixNano()), reviewers, opinions, s.now())
	if e != nil {
		return nil, e
	}
	a.RequestID = request
	a.RequestFingerprint = fingerprint
	a.Quorum = map[string]interface{}{"required": required, "approved": approved}
	for i := range p.ReleaseRounds {
		p.ReleaseRounds[i].Closed = true
	}
	if e = s.store.Update(p, 0); e != nil {
		return nil, e
	}
	s.event(id, "released_archived", actor, request, map[string]interface{}{"archive_id": a.ID})
	if request != "" {
		s.idem["release:"+request] = a
	}
	return a, nil
}
func (s *Service) GetProject(id string) (*domain.RestorationProject, error) { return s.store.Get(id) }
func (s *Service) ListProjects() []*domain.RestorationProject               { return s.store.List() }
func (s *Service) ListProjectsFiltered(f persistence.ProjectFilter, page, size int) (items []*domain.RestorationProject, total int, stats map[domain.ProjectStatus]int) {
	all := s.store.ListFiltered(f)
	total = len(all)
	stats = map[domain.ProjectStatus]int{}
	for _, p := range all {
		stats[p.Status]++
	}
	if size <= 0 {
		return nil, total, stats
	}
	start := (page - 1) * size
	if start >= len(all) {
		return []*domain.RestorationProject{}, total, stats
	}
	end := start + size
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, stats
}
func (s *Service) AuditQuery(actor, request, summary string) {
	s.event("", "project_search", actor, request, map[string]interface{}{"filters": summary})
}

func (s *Service) AuditQueryProject(projectID, actor, request, summary string) {
	s.event(projectID, "audit_query", actor, request, map[string]interface{}{"filters": summary})
}

type ProcedureView struct {
	ID                    string `json:"id"`
	Sequence              int    `json:"sequence"`
	Name                  string `json:"name"`
	Technician            string `json:"technician"`
	Completed             bool   `json:"completed"`
	PrerequisiteCompleted bool   `json:"prerequisite_completed"`
	Executable            bool   `json:"executable"`
	BlockedReason         string `json:"blocked_reason,omitempty"`
}

func (s *Service) ProcedureSummary(p *domain.RestorationProject, technician string, pending bool) (map[string]interface{}, error) {
	items := make([]ProcedureView, 0, len(p.Procedures))
	completed := 0
	next := ""
	for i, pr := range p.Procedures {
		if technician != "" && pr.Technician != technician {
			continue
		}
		pre := i == 0 || p.Procedures[i-1].Completed
		executable := !pr.Completed && pre
		reason := ""
		if pr.Completed {
			completed++
		} else if !pre {
			reason = "前置工序未完成"
		}
		if next == "" && !pr.Completed {
			next = pr.ID
		}
		if pending && pr.Completed {
			continue
		}
		items = append(items, ProcedureView{pr.ID, pr.Sequence, pr.Name, pr.Technician, pr.Completed, pre, executable, reason})
	}
	pct := 0.0
	if len(p.Procedures) > 0 {
		pct = float64(completed) * 100 / float64(len(p.Procedures))
	}
	return map[string]interface{}{"procedures": items, "completed_count": completed, "total_count": len(p.Procedures), "completion_percent": pct, "next_procedure": next}, nil
}
func (s *Service) ListInspections(id, decision, inspector string, since, until *time.Time, page, size int) ([]*domain.InspectionBatch, int, map[string]int, error) {
	p, e := s.store.Get(id)
	if e != nil {
		return nil, 0, nil, e
	}
	all := make([]*domain.InspectionBatch, 0)
	stats := map[string]int{"pass": 0, "remediate": 0, "fail": 0, "missing_due": 0, "overdue_count": 0}
	now := s.now()
	for _, i := range p.Inspections {
		if decision != "" && i.Decision != decision {
			continue
		}
		if inspector != "" && i.Inspector != inspector {
			continue
		}
		if since != nil && i.CheckedAt.Before(*since) {
			continue
		}
		if until != nil && i.CheckedAt.After(*until) {
			continue
		}
		all = append(all, i)
		stats[i.Decision]++
		if i.Decision == "remediate" {
			if i.DueAt == nil {
				stats["missing_due"]++
			} else if i.DueAt.Before(now) {
				stats["overdue_count"]++
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CheckedAt.Equal(all[j].CheckedAt) {
			return all[i].ID < all[j].ID
		}
		return all[i].CheckedAt.Before(all[j].CheckedAt)
	})
	total := len(all)
	start := (page - 1) * size
	if start >= total {
		return []*domain.InspectionBatch{}, total, stats, nil
	}
	end := start + size
	if end > total {
		end = total
	}
	return all[start:end], total, stats, nil
}

func (s *Service) DefectAggregate(id, decision, procedureID string, since, until *time.Time) (map[string]interface{}, error) {
	p, e := s.store.Get(id)
	if e != nil {
		return nil, e
	}
	type agg struct {
		Count      int
		Last       time.Time
		Sources    []string
		Procedures map[string]bool
	}
	m := map[string]*agg{}
	decisionCounts := map[string]int{"pass": 0, "remediate": 0, "fail": 0}
	for _, in := range p.Inspections {
		if decision != "" && in.Decision != decision {
			continue
		}
		if since != nil && in.CheckedAt.Before(*since) || until != nil && in.CheckedAt.After(*until) {
			continue
		}
		decisionCounts[in.Decision]++
		for _, f := range in.Findings {
			key := strings.ToLower(strings.Join(strings.Fields(f), " "))
			if key == "" {
				continue
			}
			a := m[key]
			if a == nil {
				a = &agg{Procedures: map[string]bool{}}
				m[key] = a
			}
			a.Count++
			if in.CheckedAt.After(a.Last) {
				a.Last = in.CheckedAt
			}
			a.Sources = append(a.Sources, in.ID)
			for _, pid := range in.SampledProcedureIDs {
				a.Procedures[pid] = true
			}
		}
	}
	out := make([]map[string]interface{}, 0, len(m))
	for k, a := range m {
		if procedureID != "" && !a.Procedures[procedureID] {
			continue
		}
		out = append(out, map[string]interface{}{"finding": k, "count": a.Count, "last_at": a.Last, "recurrence": a.Count > 1, "inspection_ids": a.Sources, "recurrence_rate": func() float64 {
			if a.Count < 2 {
				return 0
			}
			return float64(a.Count-1) / float64(a.Count)
		}()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["finding"].(string) < out[j]["finding"].(string) })
	remediationByInspection := map[string][]string{}
	for _, r := range p.Remediations {
		remediationByInspection[r.InspectionID] = append(remediationByInspection[r.InspectionID], r.ID)
	}
	return map[string]interface{}{"defects": out, "decision_counts": decisionCounts, "remediations": remediationByInspection}, nil
}
func (s *Service) Timeline(id string) []audit.Event { return s.audit.List(id) }
func (s *Service) VerifyTimeline(id string) bool    { return s.audit.Verify(id) }
func (s *Service) VerifyTimelineDiagnostic(id string) (bool, string, string) {
	return s.audit.VerifyDiagnostic(id)
}
func (s *Service) VerifyArchives(p *domain.RestorationProject) bool {
	ids := make([]string, 0, len(p.Evidence))
	for _, e := range p.Evidence {
		if e.Superseded {
			continue
		}
		ids = append(ids, e.ID+e.SHA256)
	}
	sort.Strings(ids)
	rootSum := sha256.Sum256([]byte(strings.Join(ids, "|")))
	root := fmt.Sprintf("%x", rootSum)
	for _, a := range p.Archives {
		if a.EvidenceRoot != root {
			return false
		}
		raw := a.ID + a.EvidenceRoot + a.ArchiveVersion
		sum := sha256.Sum256([]byte(raw))
		if fmt.Sprintf("%x", sum) != a.Checksum {
			return false
		}
	}
	return true
}
