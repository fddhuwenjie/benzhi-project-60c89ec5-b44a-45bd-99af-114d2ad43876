package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid domain data")
	ErrConflict  = errors.New("revision conflict")
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("operation not allowed")
)

// FieldValidationError reports normalized input problems per field.
type FieldValidationError struct{ Fields map[string]string }

func (e *FieldValidationError) Error() string { return "字段校验失败" }
func (e *FieldValidationError) Unwrap() error { return ErrInvalid }

func ValidateProjectFields(asset, title, custodian string) (string, string, string, error) {
	vals := map[string]string{"asset_code": asset, "title": title, "custodian": custodian}
	clean := map[string]string{}
	fields := map[string]string{}
	for k, v := range vals {
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				fields[k] = "包含控制字符"
				break
			}
		}
		v = strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
		clean[k] = v
		if v == "" {
			fields[k] = "不能为空"
			continue
		}
		if len([]rune(v)) > 256 {
			fields[k] = "长度超过限制"
		}
	}
	if len(fields) > 0 {
		return "", "", "", &FieldValidationError{Fields: fields}
	}
	return clean["asset_code"], clean["title"], clean["custodian"], nil
}

// CapacityError identifies the technician whose pending workload exceeds the
// caller supplied limit while validating a procedure batch.
type CapacityError struct {
	Technician string
	Limit      int
	Count      int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("capacity_exceeded: technician=%s count=%d limit=%d", e.Technician, e.Count, e.Limit)
}
func (e *CapacityError) Unwrap() error { return ErrConflict }

// ProjectConflictError preserves the existing project identifier for API clients.
type ProjectConflictError struct {
	Reason    string
	ProjectID string
}

func (e *ProjectConflictError) Error() string { return e.Reason }
func (e *ProjectConflictError) Unwrap() error { return ErrConflict }

type ProjectStatus string

const (
	StatusDraft       ProjectStatus = "draft"
	StatusBaselined   ProjectStatus = "baselined"
	StatusInProgress  ProjectStatus = "in_progress"
	StatusInspection  ProjectStatus = "inspection"
	StatusRemediation ProjectStatus = "remediation"
	StatusReleased    ProjectStatus = "released"
	StatusArchived    ProjectStatus = "archived"
)

type RestorationProject struct {
	ID                      string             `json:"id"`
	AssetCode               string             `json:"asset_code"`
	AssetKey                string             `json:"asset_key,omitempty"`
	Title                   string             `json:"title"`
	Custodian               string             `json:"custodian"`
	PlanRevision            int                `json:"plan_revision"`
	Plan                    string             `json:"plan,omitempty"`
	Materials               []MaterialEntry    `json:"materials,omitempty"`
	RiskLevel               string             `json:"risk_level"`
	Status                  ProjectStatus      `json:"status"`
	CreatedAt               time.Time          `json:"created_at"`
	UpdatedAt               time.Time          `json:"updated_at"`
	Procedures              []*ProcedureRecord `json:"procedures,omitempty"`
	Evidence                []*EvidenceItem    `json:"evidence,omitempty"`
	EvidenceDuplicateHashes []string           `json:"evidence_duplicate_hashes,omitempty"`
	Inspections             []*InspectionBatch `json:"inspections,omitempty"`
	Remediations            []*Remediation     `json:"remediations,omitempty"`
	Archives                []*ReleaseArchive  `json:"archives,omitempty"`
	CreateRequestID         string             `json:"create_request_id,omitempty"`
	CreateFingerprint       string             `json:"create_fingerprint,omitempty"`
	BaselineRequestID       string             `json:"baseline_request_id,omitempty"`
	BaselineFingerprint     string             `json:"baseline_fingerprint,omitempty"`
	ProcedureRequestID      string             `json:"procedure_request_id,omitempty"`
	ProcedureFingerprint    string             `json:"procedure_fingerprint,omitempty"`
	BaselineHistory         []BaselineRevision `json:"baseline_history,omitempty"`
	ReleaseRounds           []ReleaseRound     `json:"release_rounds,omitempty"`
}
type ReleaseRound struct {
	Round        int               `json:"round"`
	RequestID    string            `json:"request_id"`
	Fingerprint  string            `json:"fingerprint"`
	Reviewers    []string          `json:"reviewers"`
	Opinions     map[string]string `json:"opinions"`
	EvidenceRoot string            `json:"evidence_root"`
	At           time.Time         `json:"at"`
	Closed       bool              `json:"closed"`
}
type BaselineRevision struct {
	Revision     int                      `json:"revision"`
	Plan         string                   `json:"plan"`
	Materials    []MaterialEntry          `json:"materials"`
	RiskLevel    string                   `json:"risk_level"`
	Operator     string                   `json:"operator,omitempty"`
	At           time.Time                `json:"at"`
	Changes      []map[string]interface{} `json:"changes,omitempty"`
	Approver     string                   `json:"approver,omitempty"`
	ImpactReason string                   `json:"impact_reason,omitempty"`
}
type MaterialEntry struct {
	Name     string  `json:"name"`
	Batch    string  `json:"batch"`
	Quantity float64 `json:"quantity"`
	Unit     string  `json:"unit"`
}

func (m *MaterialEntry) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		m.Name = s
		m.Batch = "legacy"
		m.Quantity = 1
		m.Unit = "piece"
		return nil
	}
	type alias MaterialEntry
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = MaterialEntry(a)
	return nil
}

type ProcedureRecord struct {
	ID                string              `json:"id"`
	ProjectID         string              `json:"project_id"`
	Sequence          int                 `json:"sequence"`
	Name              string              `json:"name"`
	Technician        string              `json:"technician"`
	StartedAt         *time.Time          `json:"started_at,omitempty"`
	EndedAt           *time.Time          `json:"ended_at,omitempty"`
	Environment       string              `json:"environment,omitempty"`
	EnvironmentParams map[string]float64  `json:"environment_params,omitempty"`
	Instruction       string              `json:"instruction,omitempty"`
	Result            string              `json:"result,omitempty"`
	Revision          int                 `json:"revision"`
	EvidenceIDs       []string            `json:"evidence_ids,omitempty"`
	Completed         bool                `json:"completed"`
	TrendStatus       string              `json:"trend_status,omitempty"`
	TrendWarnings     []string            `json:"trend_warnings,omitempty"`
	Pauses            []PauseInterval     `json:"pauses,omitempty"`
	EffectiveWork     time.Duration       `json:"effective_work_ns,omitempty"`
	ReworkSnapshots   []ProcedureSnapshot `json:"rework_snapshots,omitempty"`
}
type ProcedureSnapshot struct {
	Revision     int        `json:"revision"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Result       string     `json:"result,omitempty"`
	EvidenceIDs  []string   `json:"evidence_ids,omitempty"`
	Reason       string     `json:"reason"`
	InspectionID string     `json:"inspection_id"`
	At           time.Time  `json:"at"`
}
type PauseInterval struct {
	StartedAt         time.Time          `json:"started_at"`
	EndedAt           *time.Time         `json:"ended_at,omitempty"`
	Reason            string             `json:"reason"`
	Operator          string             `json:"operator"`
	Overdue           bool               `json:"overdue,omitempty"`
	PauseEnvironment  map[string]float64 `json:"pause_environment,omitempty"`
	ResumeEnvironment map[string]float64 `json:"resume_environment,omitempty"`
}
type EvidenceItem struct {
	ID                string            `json:"id"`
	ProjectID         string            `json:"project_id"`
	ProcedureID       string            `json:"procedure_id"`
	Kind              string            `json:"kind"`
	URI               string            `json:"uri"`
	SHA256            string            `json:"sha256"`
	CapturedAt        time.Time         `json:"captured_at"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	ReplaceOf         string            `json:"replace_of,omitempty"`
	ReplacementReason string            `json:"replacement_reason,omitempty"`
	Superseded        bool              `json:"superseded,omitempty"`
}
type InspectionBatch struct {
	ID                  string                 `json:"id"`
	ProjectID           string                 `json:"project_id"`
	Inspector           string                 `json:"inspector"`
	CheckedAt           time.Time              `json:"checked_at"`
	Decision            string                 `json:"decision"`
	Findings            []string               `json:"findings,omitempty"`
	DueAt               *time.Time             `json:"due_at,omitempty"`
	Revision            int                    `json:"revision"`
	RequestID           string                 `json:"request_id,omitempty"`
	RequestFingerprint  string                 `json:"request_fingerprint,omitempty"`
	SampledProcedureIDs []string               `json:"sampled_procedure_ids,omitempty"`
	EvidenceIDs         []string               `json:"evidence_ids,omitempty"`
	RevisionHistory     []InspectionRevision   `json:"revision_history,omitempty"`
	Frozen              bool                   `json:"frozen,omitempty"`
	Coverage            map[string]interface{} `json:"coverage,omitempty"`
}
type InspectionRevision struct {
	Revision  int       `json:"revision"`
	Decision  string    `json:"decision"`
	Inspector string    `json:"inspector"`
	Findings  []string  `json:"findings,omitempty"`
	At        time.Time `json:"at"`
}
type Remediation struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"project_id"`
	InspectionID       string     `json:"inspection_id"`
	Description        string     `json:"description"`
	Assignee           string     `json:"assignee"`
	Status             string     `json:"status"`
	DueAt              *time.Time `json:"due_at,omitempty"`
	EvidenceIDs        []string   `json:"evidence_ids,omitempty"`
	ReviewedBy         string     `json:"reviewed_by,omitempty"`
	ReviewedAt         *time.Time `json:"reviewed_at,omitempty"`
	EscalatedAt        *time.Time `json:"escalated_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	OriginalDueAt      *time.Time `json:"original_due_at,omitempty"`
	OriginalAssignee   string     `json:"original_assignee,omitempty"`
	ExtensionReason    string     `json:"extension_reason,omitempty"`
	ApprovedBy         string     `json:"approved_by,omitempty"`
	ReviewReason       string     `json:"review_reason,omitempty"`
	ExtensionCount     int        `json:"extension_count,omitempty"`
	EscalationRequired bool       `json:"escalation_required,omitempty"`
}
type ReleaseArchive struct {
	ID                 string                 `json:"id"`
	ProjectID          string                 `json:"project_id"`
	Reviewers          []string               `json:"reviewers"`
	Opinions           map[string]string      `json:"opinions"`
	ReleasedAt         time.Time              `json:"released_at"`
	EvidenceRoot       string                 `json:"evidence_root"`
	ArchiveVersion     string                 `json:"archive_version"`
	Checksum           string                 `json:"checksum"`
	RequestID          string                 `json:"request_id,omitempty"`
	RequestFingerprint string                 `json:"request_fingerprint,omitempty"`
	Quorum             map[string]interface{} `json:"quorum,omitempty"`
}

type BaselinePreflight struct {
	Missing     []map[string]string      `json:"missing"`
	Fingerprint string                   `json:"fingerprint"`
	Revision    int                      `json:"revision"`
	Changes     []map[string]interface{} `json:"changes,omitempty"`
	RiskUpgrade bool                     `json:"risk_upgrade,omitempty"`
}
type EvidenceIssue struct {
	Type        string `json:"type"`
	EvidenceID  string `json:"evidence_id,omitempty"`
	ProcedureID string `json:"procedure_id,omitempty"`
	Detail      string `json:"detail"`
}
type EvidenceVerification struct {
	Issues        []EvidenceIssue `json:"issues"`
	EvidenceCount int             `json:"evidence_count"`
	SummaryHash   string          `json:"summary_hash"`
	Current       bool            `json:"current"`
}

func NewProject(id, asset, title, custodian string, now time.Time) (*RestorationProject, error) {
	asset = strings.TrimSpace(asset)
	title = strings.TrimSpace(title)
	custodian = strings.TrimSpace(custodian)
	if strings.TrimSpace(id) == "" || asset == "" || title == "" || custodian == "" {
		return nil, ErrInvalid
	}
	asset = strings.Join(strings.Fields(asset), " ")
	return &RestorationProject{ID: id, AssetCode: asset, AssetKey: strings.ToUpper(asset), Title: title, Custodian: custodian, Status: StatusDraft, PlanRevision: 1, CreatedAt: now, UpdatedAt: now}, nil
}
func NormalizeMaterials(raw interface{}) ([]MaterialEntry, []map[string]string, error) {
	var out []MaterialEntry
	switch v := raw.(type) {
	case []string:
		for _, x := range v {
			parts := strings.Fields(x)
			if len(parts) >= 4 {
				q, _ := strconv.ParseFloat(parts[len(parts)-2], 64)
				out = append(out, MaterialEntry{Name: strings.Join(parts[:len(parts)-3], " "), Batch: parts[len(parts)-3], Quantity: q, Unit: parts[len(parts)-1]})
			} else {
				out = append(out, MaterialEntry{Name: strings.TrimSpace(x), Batch: "legacy", Quantity: 1, Unit: "piece"})
			}
		}
	case []MaterialEntry:
		out = append(out, v...)
	default:
		return nil, nil, ErrInvalid
	}
	diag := []map[string]string{}
	seen := map[string]bool{}
	for i := range out {
		m := &out[i]
		m.Name = strings.Join(strings.Fields(strings.TrimSpace(m.Name)), " ")
		m.Batch = strings.Join(strings.Fields(strings.TrimSpace(m.Batch)), " ")
		m.Unit = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(m.Unit)), " "))
		key := strings.ToLower(m.Name) + "|" + strings.ToLower(m.Batch)
		if m.Name == "" {
			diag = append(diag, map[string]string{"field": fmt.Sprintf("materials[%d].name", i), "reason": "材料名称不能为空"})
		}
		if m.Batch == "" {
			diag = append(diag, map[string]string{"field": fmt.Sprintf("materials[%d].batch", i), "reason": "批次不能为空"})
		}
		if m.Quantity <= 0 {
			diag = append(diag, map[string]string{"field": fmt.Sprintf("materials[%d].quantity", i), "reason": "用量必须为正"})
		}
		if m.Unit == "" {
			diag = append(diag, map[string]string{"field": fmt.Sprintf("materials[%d].unit", i), "reason": "单位不能为空"})
		}
		if seen[key] {
			diag = append(diag, map[string]string{"field": fmt.Sprintf("materials[%d]", i), "reason": "名称批次重复"})
		}
		seen[key] = true
	}
	if len(diag) > 0 {
		return out, diag, ErrInvalid
	}
	return out, nil, nil
}
func (p *RestorationProject) SetBaseline(plan string, materials interface{}, risk string, now time.Time) error {
	plan = strings.Join(strings.Fields(plan), " ")
	clean, diag, _ := NormalizeMaterials(materials)
	if (p.Status != StatusDraft && p.Status != StatusBaselined) || plan == "" || len(clean) == 0 || len(diag) > 0 || !validRisk(strings.TrimSpace(risk)) {
		return ErrInvalid
	}
	if strings.TrimSpace(risk) == "high" {
		lower := strings.ToLower(plan)
		required := []struct{ marker, cn string }{{"risk_identification:", "风险识别"}, {"control_measures:", "控制措施"}, {"responsible_person:", "责任人"}, {"emergency_materials:", "应急材料"}}
		for _, req := range required {
			idx := strings.Index(lower, req.marker)
			if idx >= 0 {
				if strings.TrimSpace(lower[idx+len(req.marker):]) == "" {
					return ErrInvalid
				}
				continue
			}
			if !strings.Contains(lower, strings.ToLower(req.cn)) {
				return ErrInvalid
			}
		}
	}
	p.Plan = plan
	p.Materials = clean
	p.RiskLevel = strings.TrimSpace(risk)
	p.PlanRevision++
	p.Status = StatusBaselined
	p.UpdatedAt = now
	return nil
}

// BaselineDiff compares a candidate with the locked baseline without mutating the project.
func (p *RestorationProject) BaselineDiff(plan string, materials interface{}, risk string) (changes []map[string]interface{}, riskUpgrade bool, missing []map[string]string) {
	plan = strings.Join(strings.Fields(plan), " ")
	clean, diag, _ := NormalizeMaterials(materials)
	missing = append(missing, diag...)
	if plan == "" {
		missing = append(missing, map[string]string{"field": "plan", "reason": "方案不能为空"})
	}
	if !validRisk(strings.TrimSpace(risk)) {
		missing = append(missing, map[string]string{"field": "risk_level", "reason": "风险等级无效"})
	}
	if p.Plan != plan {
		changes = append(changes, map[string]interface{}{"type": "modified", "field": "plan", "before": p.Plan, "after": plan})
	}
	old := map[string]MaterialEntry{}
	for _, m := range p.Materials {
		old[strings.ToLower(m.Name)+"|"+strings.ToLower(m.Batch)] = m
	}
	newm := map[string]MaterialEntry{}
	for _, m := range clean {
		key := strings.ToLower(m.Name) + "|" + strings.ToLower(m.Batch)
		newm[key] = m
		if om, ok := old[key]; !ok {
			changes = append(changes, map[string]interface{}{"type": "added", "field": "materials", "after": m})
		} else if om.Quantity != m.Quantity || om.Unit != m.Unit {
			changes = append(changes, map[string]interface{}{"type": "modified", "field": "materials", "before": om, "after": m})
		}
	}
	for k, om := range old {
		if _, ok := newm[k]; !ok {
			changes = append(changes, map[string]interface{}{"type": "deleted", "field": "materials", "before": om})
		}
	}
	if p.RiskLevel != strings.TrimSpace(risk) {
		changes = append(changes, map[string]interface{}{"type": "modified", "field": "risk_level", "before": p.RiskLevel, "after": strings.TrimSpace(risk)})
		riskUpgrade = riskRank(strings.TrimSpace(risk)) > riskRank(p.RiskLevel)
	}
	return
}
func riskRank(r string) int {
	switch r {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	}
	return 0
}
func validRisk(r string) bool { return r == "low" || r == "medium" || r == "high" }
func (p *RestorationProject) AddProcedure(pr *ProcedureRecord, now time.Time) error {
	if p.Status != StatusBaselined && p.Status != StatusInProgress {
		return ErrForbidden
	}
	if pr.Sequence <= 0 || strings.TrimSpace(pr.Name) == "" || strings.TrimSpace(pr.Technician) == "" {
		return ErrInvalid
	}
	for _, x := range p.Procedures {
		if x.Sequence == pr.Sequence {
			return ErrConflict
		}
	}
	pr.ProjectID = p.ID
	pr.Revision = 1
	p.Procedures = append(p.Procedures, pr)
	sort.Slice(p.Procedures, func(i, j int) bool { return p.Procedures[i].Sequence < p.Procedures[j].Sequence })
	p.Status = StatusInProgress
	p.PlanRevision++
	p.UpdatedAt = now
	return nil
}
func (p *RestorationProject) AddProcedures(items []*ProcedureRecord, now time.Time) error {
	if len(items) == 0 {
		return ErrInvalid
	}
	if p.Status != StatusBaselined && p.Status != StatusInProgress {
		return ErrForbidden
	}
	// Validate the complete batch before changing the aggregate so a bad item
	// cannot leave a partially configured plan behind.
	ordered := append([]*ProcedureRecord(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	seenSeq := map[int]bool{}
	seenName := map[string]bool{}
	for i, pr := range ordered {
		if pr == nil {
			return ErrInvalid
		}
		nameKey := strings.ToLower(strings.TrimSpace(pr.Name))
		if pr.Sequence != i+1 || seenSeq[pr.Sequence] || seenName[nameKey] || nameKey == "" || strings.TrimSpace(pr.Technician) == "" {
			return ErrInvalid
		}
		seenSeq[pr.Sequence] = true
		pr.Name = strings.TrimSpace(pr.Name)
		pr.Technician = strings.TrimSpace(pr.Technician)
		seenName[nameKey] = true
		for _, old := range p.Procedures {
			if old.Sequence == pr.Sequence || strings.ToLower(strings.TrimSpace(old.Name)) == nameKey {
				return ErrConflict
			}
		}
	}
	for _, pr := range ordered {
		pr.ProjectID = p.ID
		if pr.ID == "" {
			return ErrInvalid
		}
		pr.Revision = 1
		p.Procedures = append(p.Procedures, pr)
	}
	sort.Slice(p.Procedures, func(i, j int) bool { return p.Procedures[i].Sequence < p.Procedures[j].Sequence })
	p.Status = StatusInProgress
	p.PlanRevision++
	p.UpdatedAt = now
	return nil
}
func (p *RestorationProject) CompleteProcedure(id string, start, end time.Time, env, instruction, result string, evidence []*EvidenceItem, now time.Time) error {
	var pr *ProcedureRecord
	for _, x := range p.Procedures {
		if x.ID == id {
			pr = x
			break
		}
	}
	if pr == nil {
		return ErrNotFound
	}
	if pr.Completed {
		return ErrConflict
	}
	if len(pr.Pauses) > 0 && pr.Pauses[len(pr.Pauses)-1].EndedAt == nil {
		return ErrConflict
	}
	limit := 72 * time.Hour
	if p.RiskLevel == "medium" {
		limit = 48 * time.Hour
	}
	if p.RiskLevel == "high" {
		limit = 24 * time.Hour
	}
	for _, pause := range pr.Pauses {
		end := end
		if pause.EndedAt != nil {
			end = *pause.EndedAt
		}
		if end.Sub(pause.StartedAt) > limit {
			return fmt.Errorf("pause_overdue")
		}
	}
	if end.Before(start) || strings.TrimSpace(env) == "" || strings.TrimSpace(result) == "" {
		return ErrInvalid
	}
	if pr.StartedAt != nil && !pr.StartedAt.Equal(start) {
		return ErrConflict
	}
	if pr.Sequence > 1 {
		for _, x := range p.Procedures {
			if x.Sequence == pr.Sequence-1 && !x.Completed {
				return ErrConflict
			}
		}
	}
	if len(evidence) == 0 {
		return ErrInvalid
	}
	params, abnormal := parseEnvironment(env, p.RiskLevel)
	if strings.HasPrefix(strings.TrimSpace(env), "{") && abnormal != "" {
		return fmt.Errorf("environment_abnormal: %s", abnormal)
	}
	trend, warnings := p.environmentTrend(pr, params)
	if trend == "blocked" {
		return fmt.Errorf("environment_trend_blocked: %s", strings.Join(warnings, ";"))
	}
	seen := map[string]bool{}
	seenIDs := map[string]bool{}
	supersede := map[string]bool{}
	for _, old := range p.Evidence {
		seen[strings.ToLower(strings.TrimSpace(old.SHA256))] = true
		seenIDs[old.ID] = true
	}
	validated := make([]*EvidenceItem, 0, len(evidence))
	for _, e := range evidence {
		if e == nil || strings.TrimSpace(e.ID) == "" || e.CapturedAt.Before(start) || e.CapturedAt.After(end) || strings.TrimSpace(e.URI) == "" || strings.TrimSpace(e.SHA256) == "" || strings.TrimSpace(e.Kind) == "" {
			return ErrInvalid
		}
		if e.ReplaceOf != "" {
			found := false
			for _, old := range p.Evidence {
				if old.ID == e.ReplaceOf && !old.Superseded {
					found = true
					supersede[old.ID] = true
					break
				}
			}
			if !found || strings.TrimSpace(e.ReplacementReason) == "" {
				return ErrConflict
			}
		}
		if seen[strings.ToLower(strings.TrimSpace(e.SHA256))] {
			if e.ReplaceOf == "" {
				for _, old := range p.Evidence {
					if strings.EqualFold(old.SHA256, e.SHA256) {
						return fmt.Errorf("duplicate_evidence: %s procedure=%s", old.ID, old.ProcedureID)
					}
				}
			}
		}
		if e.Kind == "instrument" || e.Kind == "measurement" {
			if e.Metadata == nil || strings.TrimSpace(e.Metadata["device_id"]) == "" && strings.TrimSpace(e.Metadata["device"]) == "" {
				return fmt.Errorf("evidence_issue: 设备编号缺失")
			}
			cal := e.Metadata["calibration_valid_until"]
			if cal == "" {
				cal = e.Metadata["valid_until"]
			}
			if cal == "" {
				cal = e.Metadata["calibration_expires_at"]
			}
			if cal == "" {
				return fmt.Errorf("evidence_issue: 校准截止时间缺失")
			}
			vt, er := time.Parse(time.RFC3339, cal)
			if er != nil || vt.Before(e.CapturedAt) {
				return fmt.Errorf("evidence_issue: 校准已过期")
			}
		}
		if seenIDs[e.ID] {
			return ErrInvalid
		}
		seen[strings.ToLower(strings.TrimSpace(e.SHA256))] = true
		seenIDs[e.ID] = true
		e.Kind = strings.TrimSpace(e.Kind)
		e.URI = strings.TrimSpace(e.URI)
		e.SHA256 = strings.ToLower(strings.TrimSpace(e.SHA256))
		e.ProjectID = p.ID
		e.ProcedureID = id
		validated = append(validated, e)
	}
	for _, e := range validated {
		p.Evidence = append(p.Evidence, e)
		pr.EvidenceIDs = append(pr.EvidenceIDs, e.ID)
	}
	for _, old := range p.Evidence {
		if supersede[old.ID] {
			old.Superseded = true
		}
	}
	if pr.StartedAt == nil {
		pr.StartedAt = &start
	}
	pr.EndedAt = &end
	pr.EffectiveWork = end.Sub(start)
	for _, pause := range pr.Pauses {
		pe := end
		if pause.EndedAt != nil {
			pe = *pause.EndedAt
		}
		if pe.After(pause.StartedAt) {
			pr.EffectiveWork -= pe.Sub(pause.StartedAt)
		}
	}
	pr.Environment = env
	pr.EnvironmentParams = params
	pr.TrendStatus = trend
	pr.TrendWarnings = warnings
	pr.Instruction = instruction
	pr.Result = result
	pr.Completed = true
	pr.Revision++
	p.UpdatedAt = now
	return nil
}

func (p *RestorationProject) ReopenProcedure(id, inspectionID, reason string, now time.Time) error {
	if p.Status == StatusArchived || strings.TrimSpace(inspectionID) == "" || strings.TrimSpace(reason) == "" {
		return ErrConflict
	}
	validInspection := false
	for _, in := range p.Inspections {
		if in.ID == inspectionID {
			validInspection = true
			break
		}
	}
	if !validInspection {
		return ErrConflict
	}
	for _, pr := range p.Procedures {
		if pr.ID != id {
			continue
		}
		if !pr.Completed {
			return ErrConflict
		}
		s := ProcedureSnapshot{Revision: pr.Revision, StartedAt: pr.StartedAt, EndedAt: pr.EndedAt, Result: pr.Result, EvidenceIDs: append([]string(nil), pr.EvidenceIDs...), Reason: strings.TrimSpace(reason), InspectionID: inspectionID, At: now}
		pr.ReworkSnapshots = append(pr.ReworkSnapshots, s)
		for _, eid := range pr.EvidenceIDs {
			for _, ev := range p.Evidence {
				if ev.ID == eid {
					ev.Superseded = true
				}
			}
		}
		pr.Completed = false
		pr.EndedAt = nil
		pr.Result = ""
		pr.EvidenceIDs = nil
		pr.Revision++
		p.PlanRevision++
		p.Status = StatusInProgress
		p.UpdatedAt = now
		return nil
	}
	return ErrNotFound
}

func (p *RestorationProject) environmentTrend(current *ProcedureRecord, params map[string]float64) (string, []string) {
	if params == nil {
		return "normal", nil
	}
	var prev *ProcedureRecord
	for _, x := range p.Procedures {
		if x.Completed && x.Sequence < current.Sequence && (prev == nil || x.Sequence > prev.Sequence) {
			prev = x
		}
	}
	if prev == nil || prev.EnvironmentParams == nil {
		return "normal", nil
	}
	threshold := map[string]float64{"temperature": 5, "humidity": 20, "illuminance": 500}
	warnings := []string{}
	for k, lim := range threshold {
		d := params[k] - prev.EnvironmentParams[k]
		if d < 0 {
			d = -d
		}
		if d > lim {
			warnings = append(warnings, fmt.Sprintf("%s变化%.2f超过阈值%.2f", k, d, lim))
		}
	}
	if len(warnings) == 0 {
		return "normal", nil
	}
	if p.RiskLevel == "high" {
		return "blocked", warnings
	}
	return "warning", warnings
}
func parseEnvironment(raw, risk string) (map[string]float64, string) {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil, ""
	}
	var obj map[string]interface{}
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return nil, "环境参数无法解析"
	}
	out := map[string]float64{}
	for _, k := range []string{"temperature", "humidity", "illuminance"} {
		v, ok := obj[k]
		if !ok {
			return nil, k + "缺失"
		}
		switch x := v.(type) {
		case float64:
			return nil, k + "单位缺失"
		case map[string]interface{}:
			n, ok := x["value"].(float64)
			u, _ := x["unit"].(string)
			if !ok || strings.TrimSpace(u) == "" {
				return nil, k + "单位缺失"
			}
			converted, err := normalizeEnvironmentUnit(k, n, u)
			if err != nil {
				return nil, k + err.Error()
			}
			out[k] = converted
		case string:
			parts := strings.Fields(x)
			if len(parts) != 2 {
				return nil, k + "无法解析"
			}
			n, err := strconv.ParseFloat(parts[0], 64)
			if err != nil {
				return nil, k + "无法解析"
			}
			out[k] = n
		default:
			return nil, k + "无法解析"
		}
	}
	if risk == "high" {
		if out["temperature"] < 10 || out["temperature"] > 30 {
			return out, "temperature超范围"
		}
		if out["humidity"] < 30 || out["humidity"] > 70 {
			return out, "humidity超范围"
		}
	} else {
		if out["temperature"] < 0 || out["temperature"] > 40 {
			return out, "temperature超范围"
		}
		if out["humidity"] < 0 || out["humidity"] > 100 {
			return out, "humidity超范围"
		}
	}
	return out, ""
}

func normalizeEnvironmentUnit(field string, value float64, unit string) (float64, error) {
	switch field {
	case "temperature":
		switch strings.ToLower(strings.TrimSpace(unit)) {
		case "c", "°c", "摄氏度":
			return value, nil
		case "f", "°f", "华氏度":
			return (value - 32) * 5 / 9, nil
		default:
			return 0, errors.New("单位无效")
		}
	case "humidity":
		switch strings.ToLower(strings.TrimSpace(unit)) {
		case "%", "percent", "rh":
			return value, nil
		default:
			return 0, errors.New("单位无效")
		}
	case "illuminance":
		switch strings.ToLower(strings.TrimSpace(unit)) {
		case "lux", "lx":
			return value, nil
		case "klux", "klx":
			return value * 1000, nil
		default:
			return 0, errors.New("单位无效")
		}
	}
	return value, nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func validSHA256(v string) bool { return sha256Pattern.MatchString(strings.TrimSpace(v)) }
func validEvidenceKind(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "photo", "document", "measurement", "instrument", "report":
		return true
	}
	return false
}
func validMetadata(kind string, m map[string]string) bool {
	if m == nil {
		return false
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return false
		}
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "photo" {
		return strings.TrimSpace(m["camera"]) != "" || strings.TrimSpace(m["description"]) != "" || strings.TrimSpace(m["拍摄说明"]) != ""
	}
	if kind == "instrument" || kind == "measurement" {
		return strings.TrimSpace(m["device"]) != "" || strings.TrimSpace(m["instrument"]) != "" || strings.TrimSpace(m["设备"]) != ""
	}
	return true
}

func (p *RestorationProject) BaselineCheck(plan string, materials interface{}, risk string) []map[string]string {
	missing := []map[string]string{}
	if strings.TrimSpace(plan) == "" {
		missing = append(missing, map[string]string{"field": "plan", "reason": "方案不能为空"})
	}
	if strings.TrimSpace(risk) == "high" {
		low := strings.ToLower(plan)
		for _, f := range []struct{ key, reason string }{{"risk_identification", "缺少风险识别"}, {"control_measures", "缺少控制措施"}, {"responsible_person", "缺少责任人"}, {"emergency_materials", "缺少应急材料"}} {
			marker := strings.ToLower(f.key) + ":"
			idx := strings.Index(low, marker)
			present := idx >= 0 && strings.TrimSpace(low[idx+len(marker):]) != ""
			if !present && !strings.Contains(low, strings.ToLower(strings.TrimPrefix(f.reason, "缺少"))) {
				missing = append(missing, map[string]string{"field": f.key, "reason": f.reason})
			}
		}
	}
	_, diag, err := NormalizeMaterials(materials)
	if err != nil && len(diag) == 0 {
		reason := "材料清单格式无效"
		if materials == nil {
			reason = "材料清单不能为空"
		}
		missing = append(missing, map[string]string{"field": "materials", "reason": reason})
	}
	missing = append(missing, diag...)
	return missing
}

func (p *RestorationProject) VerifyEvidence() EvidenceVerification {
	issues := []EvidenceIssue{}
	for _, h := range p.EvidenceDuplicateHashes {
		issues = append(issues, EvidenceIssue{Type: "duplicate_hash", Detail: "重复哈希: " + h})
	}
	seenHash := map[string]string{}
	byProc := map[string][]*EvidenceItem{}
	for _, e := range p.Evidence {
		if e == nil {
			continue
		}
		if e.Superseded {
			continue
		}
		if old, ok := seenHash[strings.ToLower(e.SHA256)]; ok {
			issues = append(issues, EvidenceIssue{Type: "duplicate_hash", EvidenceID: e.ID, Detail: "与" + old + "重复"})
		} else {
			seenHash[strings.ToLower(e.SHA256)] = e.ID
		}
		if !validSHA256(e.SHA256) {
			issues = append(issues, EvidenceIssue{Type: "invalid_sha256", EvidenceID: e.ID, Detail: "sha256 必须为 64 位十六进制"})
		}
		if !validEvidenceKind(e.Kind) {
			issues = append(issues, EvidenceIssue{Type: "invalid_kind", EvidenceID: e.ID, Detail: "证据类型无效"})
		}
		if !validMetadata(e.Kind, e.Metadata) {
			issues = append(issues, EvidenceIssue{Type: "metadata_missing", EvidenceID: e.ID, Detail: "证据元数据不完整"})
		}
		linked := false
		for _, pr := range p.Procedures {
			if pr.ID == e.ProcedureID {
				linked = true
				byProc[pr.ID] = append(byProc[pr.ID], e)
				if !pr.Completed {
					issues = append(issues, EvidenceIssue{Type: "uncompleted_procedure", EvidenceID: e.ID, ProcedureID: pr.ID, Detail: "工序未完成"})
				}
				if pr.EndedAt != nil && e.CapturedAt.After(*pr.EndedAt) {
					issues = append(issues, EvidenceIssue{Type: "captured_after_end", EvidenceID: e.ID, ProcedureID: pr.ID, Detail: "采集时间晚于工序结束"})
				}
				break
			}
		}
		if !linked {
			issues = append(issues, EvidenceIssue{Type: "orphan", EvidenceID: e.ID, Detail: "未关联工序"})
		}
	}
	for pid, es := range byProc {
		for i := 1; i < len(es); i++ {
			if es[i].CapturedAt.Before(es[i-1].CapturedAt) {
				issues = append(issues, EvidenceIssue{Type: "time_reverse", ProcedureID: pid, Detail: "采集时间倒序"})
				break
			}
		}
	}
	ids := make([]string, 0, len(p.Evidence))
	for _, e := range p.Evidence {
		ids = append(ids, e.ID+e.SHA256+e.ProcedureID)
	}
	ids = append(ids, p.EvidenceDuplicateHashes...)
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "|")))
	count := 0
	for _, e := range p.Evidence {
		if e != nil && !e.Superseded {
			count++
		}
	}
	return EvidenceVerification{Issues: issues, EvidenceCount: count, SummaryHash: hex.EncodeToString(h[:]), Current: len(issues) == 0}
}
func (p *RestorationProject) AddInspection(i *InspectionBatch, now time.Time) error {
	if p.Status != StatusInProgress && p.Status != StatusInspection && p.Status != StatusRemediation {
		return ErrForbidden
	}
	i.Decision = strings.TrimSpace(i.Decision)
	i.Inspector = strings.TrimSpace(i.Inspector)
	for n := range i.Findings {
		i.Findings[n] = strings.TrimSpace(i.Findings[n])
	}
	cleanFindings := i.Findings[:0]
	for _, finding := range i.Findings {
		if finding != "" {
			cleanFindings = append(cleanFindings, finding)
		}
	}
	i.Findings = cleanFindings
	if i.Decision != "pass" && i.Decision != "remediate" && i.Decision != "fail" {
		return ErrInvalid
	}
	if strings.TrimSpace(i.Inspector) == "" {
		return ErrInvalid
	}
	// When a sample is supplied, only sampled procedures must be complete;
	// unsampled work may still be in progress.  An empty sample retains the
	// legacy full-project inspection semantics.
	if len(i.SampledProcedureIDs) == 0 {
		for _, pr := range p.Procedures {
			if !pr.Completed {
				return ErrConflict
			}
			if len(pr.Pauses) > 0 && pr.Pauses[len(pr.Pauses)-1].EndedAt == nil {
				return ErrConflict
			}
		}
	}
	if len(i.SampledProcedureIDs) > 0 {
		seenP := map[string]bool{}
		for _, pid := range i.SampledProcedureIDs {
			if seenP[pid] {
				return ErrConflict
			}
			seenP[pid] = true
			found := false
			for _, pr := range p.Procedures {
				if pr.ID == pid && pr.Completed {
					found = true
				}
			}
			if !found {
				return ErrConflict
			}
			for _, pr := range p.Procedures {
				if pr.ID == pid && len(pr.Pauses) > 0 && pr.Pauses[len(pr.Pauses)-1].EndedAt == nil {
					return ErrConflict
				}
			}
		}
		if i.Decision == "pass" {
			req := 1
			if p.RiskLevel == "high" {
				req = len(p.Procedures)
			} else if p.RiskLevel == "medium" {
				req = (len(p.Procedures) + 1) / 2
			}
			if len(i.SampledProcedureIDs) < req {
				return ErrConflict
			}
			for _, pid := range i.SampledProcedureIDs {
				has := false
				for _, eid := range i.EvidenceIDs {
					for _, ev := range p.Evidence {
						if ev.ID == eid && ev.ProcedureID == pid && !ev.Superseded {
							has = true
						}
					}
				}
				if !has {
					return fmt.Errorf("evidence_missing: %s", pid)
				}
			}
		}
		seenE := map[string]bool{}
		for _, eid := range i.EvidenceIDs {
			if seenE[eid] {
				return ErrConflict
			}
			seenE[eid] = true
			found := false
			for _, e := range p.Evidence {
				if e.ID == eid {
					found = true
					if !seenP[e.ProcedureID] {
						return ErrConflict
					}
				}
			}
			if !found {
				return ErrConflict
			}
		}
	}
	if len(i.EvidenceIDs) > 0 && len(i.SampledProcedureIDs) == 0 {
		return ErrConflict
	}
	if i.Decision == "pass" && (len(i.Findings) > 0 || i.DueAt != nil) {
		return ErrInvalid
	}
	// The sampling-count threshold is only meaningful when a sample list is
	// explicitly supplied.  When no sample is provided the completeness check
	// above already validated every procedure, which is the gate for the
	// legacy full-project inspection; the risk-tier sample quota must not
	// block that path.
	if i.Decision == "pass" && len(i.SampledProcedureIDs) > 0 {
		required := 1
		if p.RiskLevel == "high" {
			required = len(p.Procedures)
		}
		if p.RiskLevel == "medium" {
			required = (len(p.Procedures) + 1) / 2
		}
		if len(i.SampledProcedureIDs) < required {
			return ErrConflict
		}
	}
	if i.Decision == "remediate" && (len(i.Findings) == 0 || i.DueAt == nil) {
		return ErrInvalid
	}
	if i.Decision == "remediate" && i.DueAt != nil {
		window := 14 * 24 * time.Hour
		if p.RiskLevel == "medium" {
			window = 7 * 24 * time.Hour
		}
		if p.RiskLevel == "high" {
			window = 3 * 24 * time.Hour
		}
		if i.DueAt.Before(now) || i.DueAt.After(now.Add(window)) {
			return fmt.Errorf("due_at_out_of_window")
		}
	}
	if i.Decision == "fail" && (len(i.Findings) == 0 || i.DueAt != nil) {
		return ErrInvalid
	}
	if i.Decision != "remediate" && i.DueAt != nil {
		return ErrInvalid
	}
	i.ProjectID = p.ID
	i.Revision = 1
	i.CheckedAt = now
	p.Inspections = append(p.Inspections, i)
	// Capture deterministic coverage at creation so freezing cannot be
	// influenced by later evidence additions.
	denom := len(p.Procedures)
	if denom == 0 {
		denom = 1
	}
	coverage := float64(len(i.SampledProcedureIDs)) / float64(denom)
	evidenceCoverage := 0.0
	if len(i.SampledProcedureIDs) > 0 {
		evidenceCoverage = float64(len(i.EvidenceIDs)) / float64(len(i.SampledProcedureIDs))
	}
	i.Coverage = map[string]interface{}{"procedure_ratio": coverage, "evidence_ratio": evidenceCoverage, "sampled_procedures": len(i.SampledProcedureIDs), "total_procedures": len(p.Procedures), "evidence": len(i.EvidenceIDs)}
	if i.Decision == "pass" {
		p.Status = StatusInspection
	} else {
		p.Status = StatusRemediation
	}
	p.UpdatedAt = now
	return nil
}
func (p *RestorationProject) AddRemediation(r *Remediation, now time.Time) error {
	if p.Status != StatusRemediation {
		return ErrForbidden
	}
	if strings.TrimSpace(r.Description) == "" || strings.TrimSpace(r.Assignee) == "" {
		return ErrInvalid
	}
	var ins *InspectionBatch
	for _, i := range p.Inspections {
		if i.ID == r.InspectionID {
			ins = i
			break
		}
	}
	if ins == nil || (ins.Decision != "remediate" && ins.Decision != "fail") {
		return ErrInvalid
	}
	if r.DueAt == nil || (ins.DueAt != nil && r.DueAt.After(*ins.DueAt)) {
		return ErrInvalid
	}
	r.ProjectID = p.ID
	r.CreatedAt = now
	r.Status = "open"
	p.Remediations = append(p.Remediations, r)
	p.UpdatedAt = now
	return nil
}
func (p *RestorationProject) ResolveRemediation(id string, evidence []string, reviewer string, now time.Time) error {
	for _, r := range p.Remediations {
		if r.ID == id {
			if r.Status == "closed" {
				return ErrConflict
			}
			if len(evidence) == 0 || strings.TrimSpace(reviewer) == "" || strings.TrimSpace(reviewer) == strings.TrimSpace(r.Assignee) {
				return ErrInvalid
			}
			seen := map[string]bool{}
			valid := make([]string, 0, len(evidence))
			for _, eid := range evidence {
				if seen[eid] {
					continue
				}
				seen[eid] = true
				found := false
				for _, e := range p.Evidence {
					if e.ID == eid && e.ProjectID == p.ID {
						for _, pr := range p.Procedures {
							if pr.ID == e.ProcedureID && pr.Completed {
								if !r.CreatedAt.IsZero() && !e.CapturedAt.After(r.CreatedAt) {
									return ErrInvalid
								}
								found = true
								break
							}
						}
						if found {
							break
						}
					}
				}
				if !found {
					return ErrInvalid
				}
				valid = append(valid, eid)
			}
			if len(valid) == 0 {
				return ErrInvalid
			}
			r.EvidenceIDs = valid
			r.ReviewedBy = reviewer
			r.Status = "closed"
			r.ReviewedAt = &now
			p.UpdatedAt = now
			return nil
		}
	}
	return ErrNotFound
}
func (p *RestorationProject) Release(id string, reviewers []string, opinions map[string]string, now time.Time) (*ReleaseArchive, error) {
	if p.Status == StatusArchived {
		return nil, ErrConflict
	}
	if p.Status != StatusInspection {
		return nil, ErrConflict
	}
	if len(reviewers) < 2 || len(opinions) < 2 {
		return nil, ErrInvalid
	}
	seenR := map[string]bool{}
	if len(reviewers) != len(opinions) {
		return nil, ErrInvalid
	}
	cleanReviewers := make([]string, 0, len(reviewers))
	for _, r := range reviewers {
		r = strings.TrimSpace(r)
		if r == "" || seenR[r] || strings.TrimSpace(opinions[r]) == "" {
			return nil, ErrInvalid
		}
		seenR[r] = true
		op := strings.ToLower(strings.TrimSpace(opinions[r]))
		if op == "同意" || op == "通过" {
			op = "approve"
		}
		if op == "拒绝" {
			op = "reject"
		}
		if op == "弃权" {
			op = "abstain"
		}
		if op != "approve" && op != "reject" && op != "abstain" {
			return nil, ErrInvalid
		}
		opinions[r] = op
		cleanReviewers = append(cleanReviewers, r)
	}
	approve := 0
	for _, op := range opinions {
		if op == "reject" {
			return nil, ErrConflict
		}
		if op == "approve" {
			approve++
		}
	}
	if approve < 2 {
		return nil, ErrConflict
	}
	for _, r := range p.Remediations {
		if r.Status != "closed" {
			return nil, ErrConflict
		}
	}
	for _, pr := range p.Procedures {
		if !pr.Completed {
			return nil, ErrConflict
		}
	}
	if len(p.Procedures) == 0 {
		return nil, ErrConflict
	}
	if len(p.Evidence) == 0 {
		return nil, ErrConflict
	}
	for _, e := range p.Evidence {
		linked := false
		for _, pr := range p.Procedures {
			if pr.ID == e.ProcedureID && pr.Completed {
				linked = true
				break
			}
		}
		if !linked {
			return nil, ErrConflict
		}
	}
	ids := make([]string, 0, len(p.Evidence))
	for _, e := range p.Evidence {
		ids = append(ids, e.ID+e.SHA256)
	}
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "|")))
	a := &ReleaseArchive{ID: id, ProjectID: p.ID, Reviewers: cleanReviewers, Opinions: opinions, ReleasedAt: now, EvidenceRoot: hex.EncodeToString(h[:]), ArchiveVersion: fmt.Sprintf("v%d", len(p.Archives)+1)}
	raw := a.ID + a.EvidenceRoot + a.ArchiveVersion
	sum := sha256.Sum256([]byte(raw))
	a.Checksum = hex.EncodeToString(sum[:])
	p.Archives = append(p.Archives, a)
	p.Status = StatusArchived
	p.UpdatedAt = now
	return a, nil
}
