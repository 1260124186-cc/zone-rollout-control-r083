package domain

import (
	"sort"
	"time"
)

const (
	ReasonPlanNotTerminal = "plan_not_terminal"
	ReasonPlanMissing     = "plan_missing"
)

func CloseCampaign(campaign *ReleaseCampaign, now time.Time) error {
	if campaign.State != CampaignOpen {
		return InvalidState("campaign is already closed", map[string]any{
			"campaign_id": campaign.ID,
			"state":       campaign.State,
		})
	}
	campaign.State = CampaignClosed
	campaign.ClosedAt = timePointer(now.UTC())
	campaign.Version++
	return nil
}

// OrderedPlanIDs returns member plan IDs in a deterministic topological
// order. Members whose predecessors are already placed come first; ties are
// broken by addition order. If the declared after-constraints contain a
// cycle, the sorted IDs of the members that cannot be ordered are returned
// instead.
func OrderedPlanIDs(members []CampaignMember) (ordered []string, cyclic []string) {
	position := make(map[string]int, len(members))
	indegree := make(map[string]int, len(members))
	dependents := make(map[string][]string, len(members))
	for index, member := range members {
		position[member.PlanID] = index
		indegree[member.PlanID] = len(member.After)
		for _, predecessor := range member.After {
			dependents[predecessor] = append(dependents[predecessor], member.PlanID)
		}
	}
	ready := make([]string, 0, len(members))
	for _, member := range members {
		if indegree[member.PlanID] == 0 {
			ready = append(ready, member.PlanID)
		}
	}
	sortReady := func() {
		sort.Slice(ready, func(i, j int) bool {
			return position[ready[i]] < position[ready[j]]
		})
	}
	sortReady()
	ordered = make([]string, 0, len(members))
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		ordered = append(ordered, next)
		for _, dependent := range dependents[next] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
		sortReady()
	}
	if len(ordered) == len(members) {
		return ordered, nil
	}
	cyclic = make([]string, 0, len(members)-len(ordered))
	for _, member := range members {
		if indegree[member.PlanID] > 0 {
			cyclic = append(cyclic, member.PlanID)
		}
	}
	sort.Strings(cyclic)
	return nil, cyclic
}

// BuildCampaignProgress derives a read-only aggregation of member plan
// states. It never mutates the campaign or the plans.
func BuildCampaignProgress(
	campaign ReleaseCampaign,
	plans map[string]RolloutPlan,
	assessments map[string]RolloutAssessment,
) (CampaignProgress, error) {
	ordered, cyclic := OrderedPlanIDs(campaign.Members)
	if len(cyclic) > 0 {
		return CampaignProgress{}, NewError(CodeStorageFailure, "campaign member order contains a cycle", map[string]any{
			"campaign_id": campaign.ID,
			"members":     cyclic,
		})
	}
	membersByID := make(map[string]CampaignMember, len(campaign.Members))
	for _, member := range campaign.Members {
		membersByID[member.PlanID] = member
	}
	progress := CampaignProgress{
		CampaignID: campaign.ID,
		State:      campaign.State,
		PlanCount:  len(campaign.Members),
		Blocking:   []CampaignBlocker{},
		Plans:      []CampaignPlanProgress{},
	}
	for position, planID := range ordered {
		member := membersByID[planID]
		plan, ok := plans[planID]
		if !ok {
			progress.Blocking = append(progress.Blocking, CampaignBlocker{
				PlanID:   planID,
				Position: position,
				Reason:   ReasonPlanMissing,
			})
			progress.Plans = append(progress.Plans, CampaignPlanProgress{
				PlanID:   planID,
				Position: position,
				After:    member.After,
			})
			continue
		}
		switch plan.State {
		case PlanDraft:
			progress.DraftCount++
		case PlanActive:
			progress.ActiveCount++
		case PlanCompleted:
			progress.CompletedCount++
		case PlanAborted:
			progress.AbortedCount++
		}
		entry := CampaignPlanProgress{
			PlanID:   planID,
			Position: position,
			Zone:     plan.Zone,
			State:    plan.State,
			After:    member.After,
		}
		if plan.LatestAssessmentID != "" {
			if assessment, found := assessments[plan.LatestAssessmentID]; found {
				entry.LatestVerdict = assessment.Verdict
			}
		}
		if plan.State != PlanCompleted && plan.State != PlanAborted {
			progress.Blocking = append(progress.Blocking, CampaignBlocker{
				PlanID:   planID,
				Position: position,
				State:    plan.State,
				Reason:   ReasonPlanNotTerminal,
			})
		}
		progress.Plans = append(progress.Plans, entry)
	}
	progress.TerminalCount = progress.CompletedCount + progress.AbortedCount
	progress.Closable = campaign.State == CampaignOpen && len(progress.Blocking) == 0
	return progress, nil
}
