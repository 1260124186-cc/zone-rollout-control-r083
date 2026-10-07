package campaign

import (
	"context"
	"sort"
	"strings"

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

func (s *Service) Create(ctx context.Context, input domain.CampaignCreateInput) (domain.CampaignView, error) {
	if err := domain.ValidateCampaignInput(input); err != nil {
		return domain.CampaignView{}, err
	}
	input = domain.NormalizeCampaignInput(input)
	now := s.clock.Now().UTC()
	campaign := domain.ReleaseCampaign{
		ID:        s.ids.New("cmp"),
		Name:      input.Name,
		Owner:     input.Owner,
		State:     domain.CampaignOpen,
		PlanIDs:   []string{},
		Version:   1,
		CreatedAt: now,
		OpenedAt:  now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		state.Campaigns[campaign.ID] = campaign
		store.AddAudit(state, s.ids, "campaign", campaign.ID, "created", now, map[string]any{
			"name":  campaign.Name,
			"owner": campaign.Owner,
		})
		return nil
	})
	if err != nil {
		return domain.CampaignView{}, err
	}
	return s.Get(ctx, campaign.ID)
}

func (s *Service) Get(ctx context.Context, campaignID string) (domain.CampaignView, error) {
	var view domain.CampaignView
	err := s.store.Read(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		view = domain.BuildCampaignView(state, campaign, s.clock.Now().UTC())
		return nil
	})
	return view, err
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

// JoinPlans appends plans to the campaign in the given order. Every plan must
// exist, must not already be linked to any campaign, and must not be repeated
// within the request; a campaign can hold at most 100 members.
func (s *Service) JoinPlans(
	ctx context.Context,
	campaignID string,
	input domain.CampaignJoinPlansInput,
) (domain.CampaignView, error) {
	if err := domain.ValidateCampaignJoinInput(input); err != nil {
		return domain.CampaignView{}, err
	}
	planIDs := make([]string, 0, len(input.PlanIDs))
	for _, raw := range input.PlanIDs {
		planIDs = append(planIDs, strings.TrimSpace(raw))
	}
	now := s.clock.Now().UTC()
	err := s.store.Write(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if campaign.State != domain.CampaignOpen {
			return domain.NewError(domain.CodeCampaignNotOpen, "campaign must be open to join plans", map[string]any{
				"campaign_id": campaignID,
				"state":       campaign.State,
			})
		}
		if len(campaign.PlanIDs)+len(planIDs) > domain.MaxCampaignPlanMembers {
			return domain.NewError(domain.CodeDuplicatePlanJoin, "campaign cannot hold more than 100 plans", map[string]any{
				"campaign_id": campaignID,
				"current":     len(campaign.PlanIDs),
				"requested":   len(planIDs),
			})
		}
		existing := make(map[string]struct{}, len(campaign.PlanIDs))
		for _, planID := range campaign.PlanIDs {
			existing[planID] = struct{}{}
		}
		requested := make(map[string]struct{}, len(planIDs))
		nextPosition := len(campaign.PlanIDs)
		for _, planID := range planIDs {
			if _, duplicatedInRequest := requested[planID]; duplicatedInRequest {
				return domain.NewError(domain.CodeDuplicatePlanJoin, "plan appears more than once in the request", map[string]any{
					"campaign_id": campaignID,
					"plan_id":     planID,
				})
			}
			requested[planID] = struct{}{}
			if _, alreadyMember := existing[planID]; alreadyMember {
				return domain.NewError(domain.CodeDuplicatePlanJoin, "plan is already linked to this campaign", map[string]any{
					"campaign_id": campaignID,
					"plan_id":     planID,
				})
			}
			if ownerCampaignID, linked := state.PlanCampaignIndex[planID]; linked {
				return domain.NewError(domain.CodeDuplicatePlanJoin, "plan is already linked to another campaign", map[string]any{
					"campaign_id":       campaignID,
					"plan_id":           planID,
					"other_campaign_id": ownerCampaignID,
				})
			}
			plan, exists := state.Plans[planID]
			if !exists {
				return domain.NotFound("plan", planID)
			}
			state.CampaignMembers[domain.CampaignMembershipKey(campaignID, planID)] = domain.CampaignMembership{
				CampaignID: campaignID,
				PlanID:     planID,
				Position:   nextPosition,
				State:      domain.MemberActive,
				JoinedAt:   now,
			}
			state.PlanCampaignIndex[planID] = campaignID
			campaign.PlanIDs = append(campaign.PlanIDs, planID)
			nextPosition++
			store.AddAudit(state, s.ids, "campaign_membership", planID, "joined", now, map[string]any{
				"campaign_id": campaignID,
				"plan_zone":   plan.Zone,
				"position":    nextPosition - 1,
			})
		}
		campaign.Version++
		state.Campaigns[campaignID] = campaign
		return nil
	})
	if err != nil {
		return domain.CampaignView{}, err
	}
	return s.Get(ctx, campaignID)
}

// Reorder replaces the total order of members. The new order must contain
// exactly the current member set with no repeats; cycles are therefore
// structurally impossible and every malformed order is rejected.
func (s *Service) Reorder(
	ctx context.Context,
	campaignID string,
	input domain.CampaignReorderInput,
) (domain.CampaignView, error) {
	if err := domain.ValidateCampaignReorderInput(input); err != nil {
		return domain.CampaignView{}, err
	}
	ordered := make([]string, 0, len(input.PlanOrder))
	for _, raw := range input.PlanOrder {
		ordered = append(ordered, strings.TrimSpace(raw))
	}
	now := s.clock.Now().UTC()
	err := s.store.Write(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if campaign.State != domain.CampaignOpen {
			return domain.NewError(domain.CodeCampaignNotOpen, "campaign must be open to reorder plans", map[string]any{
				"campaign_id": campaignID,
				"state":       campaign.State,
			})
		}
		if err := domain.ValidatePlanOrder(campaign.PlanIDs, ordered); err != nil {
			return err
		}
		for position, planID := range ordered {
			membership, found := state.CampaignMembers[domain.CampaignMembershipKey(campaignID, planID)]
			if !found {
				return domain.NewError(domain.CodeStorageFailure, "campaign membership is missing", map[string]any{
					"campaign_id": campaignID,
					"plan_id":     planID,
				})
			}
			membership.Position = position
			state.CampaignMembers[domain.CampaignMembershipKey(campaignID, planID)] = membership
		}
		campaign.PlanIDs = ordered
		campaign.Version++
		state.Campaigns[campaignID] = campaign
		store.AddAudit(state, s.ids, "campaign", campaignID, "reordered", now, map[string]any{
			"plan_count": len(ordered),
		})
		return nil
	})
	if err != nil {
		return domain.CampaignView{}, err
	}
	return s.Get(ctx, campaignID)
}

// AbortPlan marks a single membership aborted at campaign scope. It never
// changes the linked plan: plan abort, completion, and campaign close remain
// independent state machines that are reconciled through the aggregate.
func (s *Service) AbortPlan(
	ctx context.Context,
	campaignID string,
	planID string,
	input domain.CampaignAbortPlanInput,
) (domain.CampaignView, error) {
	if err := domain.ValidateCampaignAbortInput(input); err != nil {
		return domain.CampaignView{}, err
	}
	input.Reason = strings.TrimSpace(input.Reason)
	planID = strings.TrimSpace(planID)
	now := s.clock.Now().UTC()
	err := s.store.Write(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if campaign.State != domain.CampaignOpen {
			return domain.NewError(domain.CodeCampaignNotOpen, "campaign must be open to abort plans", map[string]any{
				"campaign_id": campaignID,
				"state":       campaign.State,
			})
		}
		membership, found := state.CampaignMembers[domain.CampaignMembershipKey(campaignID, planID)]
		if !found {
			return domain.NotFound("campaign membership", planID)
		}
		if err := domain.AbortCampaignMembership(&membership, input.Reason, now); err != nil {
			return err
		}
		state.CampaignMembers[domain.CampaignMembershipKey(campaignID, planID)] = membership
		campaign.Version++
		state.Campaigns[campaignID] = campaign
		store.AddAudit(state, s.ids, "campaign_membership", planID, "aborted", now, map[string]any{
			"campaign_id": campaignID,
			"reason":      input.Reason,
		})
		return nil
	})
	if err != nil {
		return domain.CampaignView{}, err
	}
	return s.Get(ctx, campaignID)
}

// Reconcile recomputes the read-only aggregate from current plan state and
// snapshots it on the campaign. It never modifies plans, so it is the retry
// path that converges any partial coordination between plan aborts, plan
// completions, and a pending campaign close.
func (s *Service) Reconcile(ctx context.Context, campaignID string) (domain.CampaignView, error) {
	now := s.clock.Now().UTC()
	err := s.store.Write(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if campaign.State != domain.CampaignOpen {
			return domain.NewError(domain.CodeCampaignNotOpen, "closed campaigns cannot be reconciled", map[string]any{
				"campaign_id": campaignID,
				"state":       campaign.State,
			})
		}
		view := domain.BuildCampaignView(state, campaign, now)
		progress := view.Progress
		campaign.LatestProgress = &progress
		reconciledAt := now
		campaign.ReconciledAt = &reconciledAt
		campaign.Version++
		state.Campaigns[campaignID] = campaign
		store.AddAudit(state, s.ids, "campaign", campaignID, "reconciled", now, map[string]any{
			"completed": progress.CompletedPlans,
			"aborted":   progress.AbortedPlans,
			"pending":   progress.PendingPlans,
		})
		return nil
	})
	if err != nil {
		return domain.CampaignView{}, err
	}
	return s.Get(ctx, campaignID)
}

// Close coordinates a campaign shut-down atomically: it reconciles first and
// only then transitions open -> closed. If any non-aborted member is still in
// flight the whole operation aborts without partial writes and returns the
// current blockers, so callers can resume plans and retry close later.
//
// Concurrency is deterministic: store writes are serialized by a single mutex.
// Two concurrent closes cannot both mutate; the stale one loses the optimistic
// version check, while a close against an already closed campaign is an
// idempotent replay of the current aggregate.
func (s *Service) Close(
	ctx context.Context,
	campaignID string,
	expectedVersion uint64,
) (domain.CampaignView, error) {
	now := s.clock.Now().UTC()
	err := s.store.Write(ctx, func(state *domain.State) error {
		campaign, ok := state.Campaigns[campaignID]
		if !ok {
			return domain.NotFound("campaign", campaignID)
		}
		if expectedVersion != 0 && campaign.Version != expectedVersion {
			return versionConflict(campaignID, expectedVersion, campaign.Version)
		}
		if campaign.State == domain.CampaignClosed {
			// A version-less (or matching-version) call against a closed
			// campaign is an idempotent replay, not a second transition.
			return nil
		}
		view := domain.BuildCampaignView(state, campaign, now)
		if !view.Progress.ReadyToClose {
			return domain.NewError(domain.CodeCampaignNotClosable, "campaign has unfinished plans and cannot close", map[string]any{
				"campaign_id":      campaignID,
				"blocking_reasons": view.Progress.BlockingReasons,
				"completed_plans":  view.Progress.CompletedPlans,
				"aborted_plans":    view.Progress.AbortedPlans,
				"pending_plans":    view.Progress.PendingPlans,
			})
		}
		progress := view.Progress
		campaign.LatestProgress = &progress
		reconciledAt := now
		campaign.ReconciledAt = &reconciledAt
		if err := domain.CloseCampaign(&campaign, now); err != nil {
			return err
		}
		state.Campaigns[campaignID] = campaign
		store.AddAudit(state, s.ids, "campaign", campaignID, "closed", now, map[string]any{
			"completed": progress.CompletedPlans,
			"aborted":   progress.AbortedPlans,
		})
		return nil
	})
	if err != nil {
		return domain.CampaignView{}, err
	}
	return s.Get(ctx, campaignID)
}

func versionConflict(id string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "campaign version does not match", map[string]any{
		"campaign_id": id,
		"expected":    expected,
		"actual":      actual,
	})
}
