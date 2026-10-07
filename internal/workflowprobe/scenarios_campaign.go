package workflowprobe

import (
	"errors"
	"net/http"
	"sync"

	"solo-0014-zone-rollout-control/internal/domain"
)

func runReleaseCampaign() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	planOne, _, err := createActivePlan(harness, "one.release.example", 7001, 1, 1, 100)
	if err != nil {
		return err
	}
	planTwo, _, err := createActivePlan(harness, "two.release.example", 7002, 1, 1, 100)
	if err != nil {
		return err
	}
	planThree, err := createDraftPlan(harness, "three.release.example")
	if err != nil {
		return err
	}
	planFour, err := createDraftPlan(harness, "four.release.example")
	if err != nil {
		return err
	}
	planFive, err := createDraftPlan(harness, "five.release.example")
	if err != nil {
		return err
	}
	planSix, err := createDraftPlan(harness, "six.release.example")
	if err != nil {
		return err
	}

	var campaign domain.ReleaseCampaign
	if err := harness.call(http.MethodPut, "/v1/campaigns", domain.CampaignCreateInput{
		Name:  "2026.10-major-release",
		Owner: "release-lead",
	}, &campaign); err != nil {
		return err
	}
	if campaign.State != domain.CampaignOpen || campaign.Version != 1 || campaign.StartedAt.IsZero() {
		return ensure(false, "campaign was not created in open state")
	}

	if err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{
			{PlanID: planOne.ID},
			{PlanID: planTwo.ID, After: []string{planOne.ID}},
			{PlanID: planThree.ID, After: []string{planOne.ID}},
		},
	}, &campaign); err != nil {
		return err
	}
	if campaign.Version != 2 || len(campaign.Members) != 3 {
		return ensure(false, "campaign members were not added")
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{{PlanID: planTwo.ID}},
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeDuplicatePlan)); err != nil {
		return err
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{{PlanID: planFour.ID, After: []string{planFour.ID}}},
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeOrderCycle)); err != nil {
		return err
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{
			{PlanID: planFive.ID, After: []string{planSix.ID}},
			{PlanID: planSix.ID, After: []string{planFive.ID}},
		},
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeOrderCycle)); err != nil {
		return err
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{{PlanID: planFour.ID, After: []string{"pln_unknown00"}}},
	}, nil)
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{{PlanID: "pln_unknown00"}},
	}, nil)
	if err := expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound)); err != nil {
		return err
	}

	progress, err := campaignProgress(harness, campaign.ID)
	if err != nil {
		return err
	}
	if progress.Closable || progress.PlanCount != 3 || progress.ActiveCount != 2 || progress.DraftCount != 1 {
		return ensure(false, "campaign progress counts are wrong")
	}
	if err := expectBlocking(progress, []string{planOne.ID, planTwo.ID, planThree.ID}); err != nil {
		return err
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{
		"expected_version": campaign.Version,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeCampaignBlocked)); err != nil {
		return err
	}

	var completed domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+planOne.ID+"/complete", map[string]any{
		"expected_version": planOne.Version,
	}, &completed); err != nil {
		return err
	}

	var aborted domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+planTwo.ID+"/abort", map[string]any{
		"expected_version": planTwo.Version,
	}, &aborted); err != nil {
		return err
	}
	if aborted.State != domain.PlanAborted || aborted.AbortedAt == nil {
		return ensure(false, "active plan was not aborted")
	}
	err = harness.call(http.MethodPatch, "/v1/plans/"+planTwo.ID+"/abort", map[string]any{
		"expected_version": aborted.Version,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/plans/"+planOne.ID+"/abort", map[string]any{
		"expected_version": completed.Version,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	progress, err = campaignProgress(harness, campaign.ID)
	if err != nil {
		return err
	}
	if progress.TerminalCount != 2 || progress.CompletedCount != 1 || progress.AbortedCount != 1 {
		return ensure(false, "campaign terminal counts are wrong")
	}
	if err := expectBlocking(progress, []string{planThree.ID}); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{
		"expected_version": campaign.Version,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeCampaignBlocked)); err != nil {
		return err
	}

	if err := harness.call(http.MethodPatch, "/v1/plans/"+planThree.ID+"/abort", map[string]any{
		"expected_version": planThree.Version,
	}, &aborted); err != nil {
		return err
	}

	closed, err := closeCampaignConcurrently(harness, campaign.ID, campaign.Version, 6)
	if err != nil {
		return err
	}
	if closed.State != domain.CampaignClosed || closed.ClosedAt == nil || closed.Version != campaign.Version+1 {
		return ensure(false, "campaign was not closed exactly once")
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{
		"expected_version": closed.Version,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/close", map[string]any{
		"expected_version": campaign.Version,
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
		return err
	}

	err = harness.call(http.MethodPatch, "/v1/campaigns/"+campaign.ID+"/plans", domain.CampaignPlansInput{
		Members: []domain.CampaignMemberInput{{PlanID: planFour.ID}},
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	progress, err = campaignProgress(harness, campaign.ID)
	if err != nil {
		return err
	}
	if progress.State != domain.CampaignClosed || progress.TerminalCount != 3 || len(progress.Blocking) != 0 {
		return ensure(false, "closed campaign progress is wrong")
	}

	var untouched domain.RolloutPlan
	if err := harness.call(http.MethodGet, "/v1/plans/"+planOne.ID, nil, &untouched); err != nil {
		return err
	}
	if untouched.Version != completed.Version || untouched.State != domain.PlanCompleted {
		return ensure(false, "campaign close modified a member plan")
	}

	var fetched domain.ReleaseCampaign
	if err := harness.call(http.MethodGet, "/v1/campaigns/"+campaign.ID, nil, &fetched); err != nil {
		return err
	}
	if fetched.State != domain.CampaignClosed || len(fetched.Members) != 3 {
		return ensure(false, "closed campaign does not keep its members")
	}

	var page domain.CampaignPage
	if err := harness.call(http.MethodGet, "/v1/campaigns?state=closed", nil, &page); err != nil {
		return err
	}
	if len(page.Items) != 1 || page.Items[0].ID != campaign.ID {
		return ensure(false, "closed campaign is not listed")
	}

	var abortedPlans domain.PlanPage
	if err := harness.call(http.MethodGet, "/v1/plans?state=aborted", nil, &abortedPlans); err != nil {
		return err
	}
	if len(abortedPlans.Items) != 2 {
		return ensure(false, "aborted plans are not listed")
	}
	return nil
}

func createDraftPlan(harness *harness, zone string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                zone,
		MinimumSuccessRatio: 1,
		MaximumLatencyMS:    100,
		ExpectedNodes:       1,
	}, &plan)
	return plan, err
}

func campaignProgress(harness *harness, campaignID string) (domain.CampaignProgress, error) {
	var progress domain.CampaignProgress
	err := harness.call(http.MethodGet, "/v1/campaigns/"+campaignID+"/progress", nil, &progress)
	return progress, err
}

func expectBlocking(progress domain.CampaignProgress, planIDs []string) error {
	if len(progress.Blocking) != len(planIDs) {
		return ensure(false, "blocking plan count does not match")
	}
	positions := make(map[string]int, len(progress.Plans))
	for _, entry := range progress.Plans {
		positions[entry.PlanID] = entry.Position
	}
	for index, planID := range planIDs {
		blocker := progress.Blocking[index]
		if blocker.PlanID != planID || blocker.Reason != domain.ReasonPlanNotTerminal {
			return ensure(false, "blocking plans are not in campaign order")
		}
		if blocker.Position != positions[planID] {
			return ensure(false, "blocking position does not match campaign order")
		}
		if index > 0 && blocker.Position <= progress.Blocking[index-1].Position {
			return ensure(false, "blocking plans are not sorted by position")
		}
	}
	for index, entry := range progress.Plans {
		if entry.Position != index {
			return ensure(false, "campaign plans are not in deterministic order")
		}
	}
	return nil
}

func closeCampaignConcurrently(
	harness *harness,
	campaignID string,
	expectedVersion uint64,
	workers int,
) (domain.ReleaseCampaign, error) {
	type outcome struct {
		campaign domain.ReleaseCampaign
		err      error
	}
	results := make(chan outcome, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var closed domain.ReleaseCampaign
			err := harness.call(http.MethodPatch, "/v1/campaigns/"+campaignID+"/close", map[string]any{
				"expected_version": expectedVersion,
			}, &closed)
			results <- outcome{campaign: closed, err: err}
		}()
	}
	wait.Wait()
	close(results)
	var closed domain.ReleaseCampaign
	successes := 0
	conflicts := 0
	for result := range results {
		if result.err == nil {
			successes++
			closed = result.campaign
			continue
		}
		var failure *apiFailure
		if errors.As(result.err, &failure) &&
			failure.Status == http.StatusConflict &&
			failure.Code == string(domain.CodeVersionConflict) {
			conflicts++
			continue
		}
		return domain.ReleaseCampaign{}, result.err
	}
	if successes != 1 || conflicts != workers-1 {
		return domain.ReleaseCampaign{}, ensure(false, "concurrent closes were not deterministic")
	}
	return closed, nil
}
