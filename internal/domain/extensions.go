package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (p *RestorationProject) ReorderProcedures(items []*ProcedureRecord, now time.Time) error {
	if len(items) != len(p.Procedures) || len(items) == 0 {
		return ErrInvalid
	}
	byID := map[string]*ProcedureRecord{}
	for _, x := range p.Procedures {
		byID[x.ID] = x
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for i, x := range items {
		if x == nil || x.Sequence != i+1 || seen[x.ID] || strings.TrimSpace(x.Name) == "" || names[strings.ToLower(strings.TrimSpace(x.Name))] {
			return ErrInvalid
		}
		old, ok := byID[x.ID]
		if !ok || old.ProjectID != p.ID {
			return ErrNotFound
		}
		if old.Completed && x.Sequence != old.Sequence {
			return &ProjectConflictError{Reason: "已完成工序位置不可调整", ProjectID: old.ID}
		}
		seen[x.ID] = true
		names[strings.ToLower(strings.TrimSpace(x.Name))] = true
	}
	for _, x := range items {
		old := byID[x.ID]
		old.Sequence = x.Sequence
		if strings.TrimSpace(x.Technician) != "" {
			old.Technician = strings.TrimSpace(x.Technician)
		}
	}
	sort.Slice(p.Procedures, func(i, j int) bool { return p.Procedures[i].Sequence < p.Procedures[j].Sequence })
	p.PlanRevision++
	p.UpdatedAt = now
	return nil
}

func (p *RestorationProject) PauseProcedure(id, reason, operator string, at time.Time) error {
	for _, pr := range p.Procedures {
		if pr.ID == id {
			if pr.Completed || pr.StartedAt == nil || strings.TrimSpace(reason) == "" {
				return ErrConflict
			}
			if len(pr.Pauses) > 0 && pr.Pauses[len(pr.Pauses)-1].EndedAt == nil {
				return ErrConflict
			}
			if at.Before(*pr.StartedAt) {
				return ErrInvalid
			}
			var env map[string]float64
			if pr.EnvironmentParams != nil {
				env = map[string]float64{}
				for k, v := range pr.EnvironmentParams {
					env[k] = v
				}
			}
			pr.Pauses = append(pr.Pauses, PauseInterval{StartedAt: at, Reason: strings.TrimSpace(reason), Operator: operator, PauseEnvironment: env})
			p.PlanRevision++
			p.UpdatedAt = at
			return nil
		}
	}
	return ErrNotFound
}
func (p *RestorationProject) ResumeProcedure(id, operator string, at time.Time) error {
	for _, pr := range p.Procedures {
		if pr.ID == id {
			if pr.Completed || len(pr.Pauses) == 0 {
				return ErrConflict
			}
			last := &pr.Pauses[len(pr.Pauses)-1]
			if last.EndedAt != nil || at.Before(last.StartedAt) || pr.StartedAt == nil || at.Before(*pr.StartedAt) {
				return ErrInvalid
			}
			last.EndedAt = &at
			p.PlanRevision++
			p.UpdatedAt = at
			return nil
		}
	}
	return ErrNotFound
}
func (p *RestorationProject) ResumeProcedureWithEnvironment(id, operator string, at time.Time, environment string) error {
	for _, pr := range p.Procedures {
		if pr.ID == id {
			if pr.Completed || len(pr.Pauses) == 0 {
				return ErrConflict
			}
			last := &pr.Pauses[len(pr.Pauses)-1]
			if last.EndedAt != nil || at.Before(last.StartedAt) || pr.StartedAt == nil || at.Before(*pr.StartedAt) {
				return ErrInvalid
			}
			if environment == "" {
				return fmt.Errorf("environment_snapshot_required")
			}
			params, abnormal := parseEnvironment(environment, p.RiskLevel)
			if abnormal != "" {
				return fmt.Errorf("environment_abnormal: %s", abnormal)
			}
			baselineEnv := last.PauseEnvironment
			if baselineEnv == nil {
				baselineEnv = pr.EnvironmentParams
			}
			if p.RiskLevel == "high" && baselineEnv != nil {
				for _, k := range []string{"temperature", "humidity", "illuminance"} {
					d := params[k] - baselineEnv[k]
					if d < 0 {
						d = -d
					}
					lim := map[string]float64{"temperature": 5, "humidity": 20, "illuminance": 500}[k]
					if d > lim {
						return fmt.Errorf("environment_trend_blocked: %s变化超过阈值", k)
					}
				}
			}
			last.ResumeEnvironment = params
			last.EndedAt = &at
			_ = operator
			p.PlanRevision++
			p.UpdatedAt = at
			return nil
		}
	}
	return ErrNotFound
}
func (p *RestorationProject) HasOpenPause(id string) bool {
	for _, pr := range p.Procedures {
		if pr.ID == id && len(pr.Pauses) > 0 {
			return pr.Pauses[len(pr.Pauses)-1].EndedAt == nil
		}
	}
	return false
}

func (p *RestorationProject) FreezeInspection(id string, now time.Time) error {
	for _, in := range p.Inspections {
		if in.ID == id {
			if in.Frozen {
				return ErrConflict
			}
			in.Frozen = true
			if in.Coverage == nil {
				denom := len(p.Procedures)
				if denom == 0 {
					denom = 1
				}
				in.Coverage = map[string]interface{}{"procedure_ratio": float64(len(in.SampledProcedureIDs)) / float64(denom), "evidence_ratio": float64(len(in.EvidenceIDs)) / float64(maxInt(1, len(in.SampledProcedureIDs))), "sampled_procedures": len(in.SampledProcedureIDs), "total_procedures": len(p.Procedures), "evidence": len(in.EvidenceIDs)}
			}
			p.UpdatedAt = now
			return nil
		}
	}
	return ErrNotFound
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
