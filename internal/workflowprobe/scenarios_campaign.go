package workflowprobe

import (
	"net/http"
	"sync"

	"solo-0014-zone-rollout-control/internal/domain"
)

func runReleaseCampaignCoordination() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	// Create the campaign with identity and owner.
	var campaign domain.CampaignView
	if err := harness.call(http.MethodPut, "/v1/campaigns", map[string]any{
		"name":  "2026 October edge release",
		"owner": "release-captain",
	}, &campaign); err != nil {
		return err
	}
	if campaign.State != domain.CampaignOpen || campaign.Owner != "release-captain" {
		return ensure(false, "campaign did not open with the requested owner")
	}
	if campaign.OpenedAt.IsZero() || campaign.ClosedAt != nil {
		return ensure(false, "campaign start state is malformed")
	}

	// Two active plans (with revisions) and one draft plan.
	planA, _, err := createActivePlan(harness, "a.edge.example", 2026100101, 1, 1, 80)
	if err != nil {
		return err
	}
	planB, _, err := createActivePlan(harness, "b.edge.example", 2026100201, 1, 1, 80)
	if err != nil {
		return err
	}
	var planC domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                "c.edge.example",
		MinimumSuccessRatio: 1,
		MaximumLatencyMS:    80,
		ExpectedNodes:       1,
	}, &planC); err != nil {
		return err
	}

	// Join the three plans in an explicit order.
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", map[string]any{
		"plan_ids": []string{planA.ID, planB.ID, planC.ID},
	}, &campaign); err != nil {
		return err
	}
	if err := ensure(campaign.Progress.TotalPlans == 3, "aggregate should count three plans"); err != nil {
		return err
	}
	if err := ensure(campaign.Progress.CompletedPlans == 0 && campaign.Progress.PendingPlans == 3, "no plan is finished yet"); err != nil {
		return err
	}
	if err := ensure(!campaign.Progress.ReadyToClose, "campaign must not be ready to close"); err != nil {
		return err
	}
	wantPositions := map[string]int{planA.ID: 0, planB.ID: 1, planC.ID: 2}
	blockers := map[string]string{}
	for _, member := range campaign.Members {
		if member.Position != wantPositions[member.PlanID] {
			return ensure(false, "member positions do not follow the join order")
		}
		blockers[member.PlanID] = member.BlockingReason
	}
	if blockers[planA.ID] != domain.CampaignBlockerPlanInActive ||
		blockers[planB.ID] != domain.CampaignBlockerPlanInActive ||
		blockers[planC.ID] != domain.CampaignBlockerPlanInDraft {
		return ensure(false, "blocking reasons do not reflect plan states")
	}

	// Duplicate joins are rejected: within the same campaign, across campaigns,
	// and as a repeated identifier inside one request.
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", map[string]any{
		"plan_ids": []string{planA.ID},
	}, nil); err != nil {
		if err := expectFailure(err, http.StatusConflict, string(domain.CodeDuplicatePlanJoin)); err != nil {
			return err
		}
	} else {
		return ensure(false, "rejoining the same plan should conflict")
	}
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", map[string]any{
		"plan_ids": []string{planB.ID, planB.ID},
	}, nil); err != nil {
		if err := expectFailure(err, http.StatusConflict, string(domain.CodeDuplicatePlanJoin)); err != nil {
			return err
		}
	} else {
		return ensure(false, "a repeated plan in one request should conflict")
	}
	var otherCampaign domain.CampaignView
	if err := harness.call(http.MethodPut, "/v1/campaigns", map[string]any{
		"name": "parallel release", "owner": "other-captain",
	}, &otherCampaign); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+otherCampaign.ID+"/plans", map[string]any{
		"plan_ids": []string{planA.ID},
	}, nil); err != nil {
		if err := expectFailure(err, http.StatusConflict, string(domain.CodeDuplicatePlanJoin)); err != nil {
			return err
		}
	} else {
		return ensure(false, "a plan linked to one campaign cannot join another")
	}

	// Reorder to a new total order, then reject every cyclic/malformed order.
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans/order", map[string]any{
		"plan_order": []string{planC.ID, planA.ID, planB.ID},
	}, &campaign); err != nil {
		return err
	}
	wantOrder := []string{planC.ID, planA.ID, planB.ID}
	for index, member := range campaign.Members {
		if member.PlanID != wantOrder[index] || member.Position != index {
			return ensure(false, "reordered positions were not persisted")
		}
	}
	for _, order := range [][]string{
		{planC.ID, planA.ID, planA.ID}, // repeated position, wrong set size
		{planC.ID, planA.ID},           // member missing
		{planC.ID, planA.ID, "pln_does_not_exist"},
	} {
		if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans/order", map[string]any{
			"plan_order": order,
		}, nil); err != nil {
			if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidPlanOrder)); err != nil {
				return err
			}
		} else {
			return ensure(false, "a cyclic or inconsistent plan order should conflict")
		}
	}

	// Closing with unfinished plans fails as a whole and reports blockers.
	versionBeforeFailedClose := campaign.Version
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{
		"expected_version": versionBeforeFailedClose,
	}, nil); err != nil {
		if err := expectFailure(err, http.StatusUnprocessableEntity, string(domain.CodeCampaignNotClosable)); err != nil {
			return err
		}
	} else {
		return ensure(false, "campaign must not close while plans are in flight")
	}
	if err := harness.call(http.MethodGet, "/v1/campaigns/"+campaign.ID, nil, &campaign); err != nil {
		return err
	}
	if campaign.State != domain.CampaignOpen || campaign.Version != versionBeforeFailedClose {
		return ensure(false, "a rejected close must not mutate the campaign")
	}

	// Campaign-scope abort for planB: the membership is aborted but the plan's
	// own state machine is untouched.
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans/"+planB.ID+"/abort", map[string]any{
		"reason": "zone B rollout withdrawn for this release",
	}, &campaign); err != nil {
		return err
	}
	var planBAfter domain.RolloutPlan
	if err := harness.call(http.MethodGet, "/v1/plans/"+planB.ID, nil, &planBAfter); err != nil {
		return err
	}
	if planBAfter.State != domain.PlanActive {
		return ensure(false, "campaign abort must not modify the plan state machine")
	}
	if campaign.Progress.AbortedPlans != 1 || campaign.Progress.PendingPlans != 2 {
		return ensure(false, "aggregate did not count the campaign-scope abort")
	}
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans/"+planB.ID+"/abort", map[string]any{
		"reason": "second abort",
	}, nil); err != nil {
		if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
			return err
		}
	} else {
		return ensure(false, "aborting a membership twice should conflict")
	}

	// Finish planA through the plan's own API (independent state machine) and
	// reconcile: the campaign observes the completion without driving it.
	var completedA domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+planA.ID+"/complete", map[string]any{
		"expected_version": planA.Version,
	}, &completedA); err != nil {
		return err
	}
	if completedA.State != domain.PlanCompleted {
		return ensure(false, "plan completion path regressed under campaign coordination")
	}
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/reconcile", nil, &campaign); err != nil {
		return err
	}
	if campaign.Progress.CompletedPlans != 1 || campaign.Progress.AbortedPlans != 1 || campaign.Progress.PendingPlans != 1 {
		return ensure(false, "reconcile did not converge plan completion and campaign abort")
	}
	if campaign.Progress.ReadyToClose {
		return ensure(false, "draft planC must still block closure")
	}

	// Abort the remaining plan at campaign scope; now closure is unblocked.
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans/"+planC.ID+"/abort", map[string]any{
		"reason": "zone C never activated, dropped from release",
	}, &campaign); err != nil {
		return err
	}
	if !campaign.Progress.ReadyToClose || campaign.Progress.CompletionRatio != 1 {
		return ensure(false, "with every non-aborted plan complete the campaign must be closable")
	}

	// Concurrent closes are deterministic: exactly one transitions, the stale
	// caller loses the optimistic version check.
	readyVersion := campaign.Version
	var winner domain.CampaignView
	var winnerErr, loserErr error
	var wg sync.WaitGroup
	wg.Add(2)
	closeOnce := func(target *domain.CampaignView, errTarget *error) {
		defer wg.Done()
		*errTarget = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{
			"expected_version": readyVersion,
		}, target)
	}
	go closeOnce(&winner, &winnerErr)
	go closeOnce(&domain.CampaignView{}, &loserErr)
	wg.Wait()
	outcomes := map[bool]int{winnerErr == nil: 0, loserErr == nil: 0}
	outcomes[winnerErr == nil]++
	outcomes[loserErr == nil]++
	if outcomes[true] != 1 || outcomes[false] != 1 {
		return ensure(false, "concurrent closes must settle as exactly one success and one conflict")
	}
	staleErr := loserErr
	if winnerErr != nil {
		staleErr = winnerErr
	}
	if err := expectFailure(staleErr, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
		return err
	}

	// Replaying close on an already closed campaign is an idempotent read.
	var replayed domain.CampaignView
	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{}, &replayed); err != nil {
		return err
	}
	if replayed.State != domain.CampaignClosed || replayed.ClosedAt == nil {
		return ensure(false, "closed campaign replay did not return the closed aggregate")
	}

	// A closed campaign rejects every membership mutation.
	for _, request := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPatch, "/v1/campaigns/" + campaign.ID + "/plans", map[string]any{"plan_ids": []string{planC.ID}}},
		{http.MethodPatch, "/v1/campaigns/" + campaign.ID + "/plans/order", map[string]any{"plan_order": []string{planC.ID, planA.ID, planB.ID}}},
		{http.MethodPatch, "/v1/campaigns/" + campaign.ID + "/plans/" + planA.ID + "/abort", map[string]any{"reason": "too late"}},
		{http.MethodPatch, "/v1/campaigns/" + campaign.ID + "/reconcile", nil},
	} {
		if err := harness.call(request.method, request.path, request.body, nil); err != nil {
			if err := expectFailure(err, http.StatusConflict, string(domain.CodeCampaignNotOpen)); err != nil {
				return err
			}
		} else {
			return ensure(false, "mutating or reconciling a closed campaign must conflict")
		}
	}

	// Final invariant: plans keep their own states. Campaign abort never
	// completed a plan, and plan completion never closed the campaign early.
	var finalView domain.CampaignView
	if err := harness.call(http.MethodGet, "/v1/campaigns/"+campaign.ID, nil, &finalView); err != nil {
		return err
	}
	finalPlanState := map[string]domain.PlanState{}
	for _, member := range finalView.Members {
		finalPlanState[member.PlanID] = member.PlanState
	}
	if finalPlanState[planA.ID] != domain.PlanCompleted ||
		finalPlanState[planB.ID] != domain.PlanActive ||
		finalPlanState[planC.ID] != domain.PlanDraft {
		return ensure(false, "campaign coordination altered plan states")
	}
	return nil
}
