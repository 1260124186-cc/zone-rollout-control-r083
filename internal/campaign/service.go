package campaign

import (
	"context"
	"sort"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

func (s *Service) Create(ctx context.Context, input domain.CampaignCreateInput) (domain.ReleaseCampaign, error) {
	if err := domain.ValidateCampaignCreateInput(input); err != nil {
		return domain.ReleaseCampaign{}, err
	}
	input = domain.NormalizeCampaignCreateInput(input)
	now := s.clock.Now().UTC()
	campaign := domain.ReleaseCampaign{
		ID:        s.ids.New("cmp"),
		Name:      input.Name,
		Owner:     input.Owner,
		State:     domain.CampaignOpen,
		Version:   1,
		Members:   []domain.CampaignMember{},
		StartedAt: now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		state.Campaigns[campaign.ID] = campaign
		store.AddAudit(state, s.ids, "campaign", campaign.ID, "created", now, map[string]any{
			"name":  campaign.Name,
			"owner": campaign.Owner,
		})
		return nil
	})
	return campaign, err
}

func (s *Service) AddPlans(
	ctx context.Context,
	campaignID string,
	input domain.CampaignPlansInput,
) (domain.ReleaseCampaign, error) {
	input = domain.NormalizeCampaignPlansInput(input)
	if err := domain.ValidateCampaignPlansInput(input); err != nil {
		return domain.ReleaseCampaign{}, err
	}
	now := s.clock.Now().UTC()
	var campaign domain.ReleaseCampaign
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if current.State != domain.CampaignOpen {
			return domain.InvalidState("campaign members can change only while campaign is open", map[string]any{
				"campaign_id": campaignID,
				"state":       current.State,
			})
		}
		if len(current.Members)+len(input.Members) > domain.MaxCampaignPlans {
			return domain.Validation("campaign plan limit is exceeded", map[string]any{
				"campaign_id": campaignID,
				"maximum":     domain.MaxCampaignPlans,
			})
		}
		membership := make(map[string]struct{}, len(current.Members)+len(input.Members))
		for _, member := range current.Members {
			membership[member.PlanID] = struct{}{}
		}
		batch := make(map[string]struct{}, len(input.Members))
		for _, member := range input.Members {
			batch[member.PlanID] = struct{}{}
		}
		additions := make([]domain.CampaignMember, 0, len(input.Members))
		for _, member := range input.Members {
			if _, exists := state.Plans[member.PlanID]; !exists {
				return domain.NotFound("plan", member.PlanID)
			}
			if _, exists := membership[member.PlanID]; exists {
				return domain.NewError(domain.CodeDuplicatePlan, "plan is already a campaign member", map[string]any{
					"campaign_id": campaignID,
					"plan_id":     member.PlanID,
				})
			}
			for _, predecessor := range member.After {
				if _, exists := membership[predecessor]; exists {
					continue
				}
				if _, incoming := batch[predecessor]; !incoming {
					return domain.Validation("after references a plan that is not a campaign member", map[string]any{
						"plan_id": member.PlanID,
						"after":   predecessor,
					})
				}
			}
			additions = append(additions, domain.CampaignMember{
				PlanID:  member.PlanID,
				After:   append([]string{}, member.After...),
				AddedAt: now,
			})
		}
		prospective := make([]domain.CampaignMember, 0, len(current.Members)+len(additions))
		prospective = append(prospective, current.Members...)
		prospective = append(prospective, additions...)
		if _, cyclic := domain.OrderedPlanIDs(prospective); len(cyclic) > 0 {
			return domain.NewError(domain.CodeOrderCycle, "plan ordering contains a cycle", map[string]any{
				"campaign_id": campaignID,
				"members":     cyclic,
			})
		}
		current.Members = append(current.Members, additions...)
		current.Version++
		state.Campaigns[campaignID] = current
		addedIDs := make([]string, 0, len(additions))
		for _, addition := range additions {
			addedIDs = append(addedIDs, addition.PlanID)
		}
		store.AddAudit(state, s.ids, "campaign", campaignID, "plans_added", now, map[string]any{
			"plan_ids": addedIDs,
		})
		campaign = current
		return nil
	})
	return campaign, err
}

func (s *Service) Close(
	ctx context.Context,
	campaignID string,
	expectedVersion uint64,
) (domain.ReleaseCampaign, error) {
	var campaign domain.ReleaseCampaign
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if current.Version != expectedVersion {
			return domain.NewError(domain.CodeVersionConflict, "campaign version does not match", map[string]any{
				"campaign_id": campaignID,
				"expected":    expectedVersion,
				"actual":      current.Version,
			})
		}
		if current.State != domain.CampaignOpen {
			return domain.InvalidState("campaign is already closed", map[string]any{
				"campaign_id": campaignID,
				"state":       current.State,
			})
		}
		progress, err := domain.BuildCampaignProgress(current, state.Plans, state.Assessments)
		if err != nil {
			return err
		}
		if len(progress.Blocking) > 0 {
			blockingIDs := make([]string, 0, len(progress.Blocking))
			for _, blocker := range progress.Blocking {
				blockingIDs = append(blockingIDs, blocker.PlanID)
			}
			return domain.NewError(domain.CodeCampaignBlocked, "campaign has plans that are not terminal", map[string]any{
				"campaign_id":       campaignID,
				"blocking_plan_ids": blockingIDs,
			})
		}
		now := s.clock.Now().UTC()
		if err := domain.CloseCampaign(&current, now); err != nil {
			return err
		}
		state.Campaigns[campaignID] = current
		store.AddAudit(state, s.ids, "campaign", campaignID, "closed", now, map[string]any{
			"version": current.Version,
		})
		campaign = current
		return nil
	})
	return campaign, err
}

func (s *Service) Get(ctx context.Context, campaignID string) (domain.ReleaseCampaign, error) {
	var campaign domain.ReleaseCampaign
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		campaign = found
		return nil
	})
	return campaign, err
}

func (s *Service) Progress(ctx context.Context, campaignID string) (domain.CampaignProgress, error) {
	var progress domain.CampaignProgress
	err := s.store.Read(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		derived, err := domain.BuildCampaignProgress(campaign, state.Plans, state.Assessments)
		if err != nil {
			return err
		}
		progress = derived
		return nil
	})
	return progress, err
}

func (s *Service) List(
	ctx context.Context,
	stateFilter domain.CampaignState,
	limit int,
	cursor string,
) (domain.CampaignPage, error) {
	if limit < 1 || limit > 100 {
		return domain.CampaignPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.CampaignPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.CampaignPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.ReleaseCampaign, 0, len(state.Campaigns))
		for _, campaign := range state.Campaigns {
			if stateFilter != "" && campaign.State != stateFilter {
				continue
			}
			if after != "" && campaign.ID <= after {
				continue
			}
			items = append(items, campaign)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}
