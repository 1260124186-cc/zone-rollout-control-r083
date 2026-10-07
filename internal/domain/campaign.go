package domain

import (
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxCampaignNameRunes   = 120
	MaxCampaignOwnerRunes  = 120
	MaxCampaignAbortRunes  = 300
	MaxCampaignPlanMembers = 100
)

type CampaignState string

const (
	CampaignOpen   CampaignState = "open"
	CampaignClosed CampaignState = "closed"
)

type CampaignMemberState string

const (
	// MemberActive means the plan is still being rolled out under the campaign.
	MemberActive CampaignMemberState = "active"
	// MemberAborted is a campaign-scope abort. The plan's own state machine is
	// never modified by the campaign; the plan simply no longer gates closure.
	MemberAborted CampaignMemberState = "aborted"
)

// Stable per-member blocking reason codes surfaced by the read-only aggregate.
const (
	CampaignBlockerPlanNotFound = "plan_not_found"
	CampaignBlockerPlanInDraft  = "plan_in_draft"
	CampaignBlockerPlanInActive = "plan_in_active"
)

type CampaignCreateInput struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

type CampaignJoinPlansInput struct {
	PlanIDs []string `json:"plan_ids"`
}

type CampaignReorderInput struct {
	PlanOrder []string `json:"plan_order"`
}

type CampaignAbortPlanInput struct {
	Reason string `json:"reason"`
}

// ReleaseCampaign groups rollout plans that belong to one major release. The
// ordered PlanIDs slice is the authoritative total order; a total order cannot
// express a cycle, and every mutation validates the member set explicitly.
type ReleaseCampaign struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Owner     string        `json:"owner"`
	State     CampaignState `json:"state"`
	PlanIDs   []string      `json:"plan_ids"`
	Version   uint64        `json:"version"`
	CreatedAt time.Time     `json:"created_at"`
	OpenedAt  time.Time     `json:"opened_at"`
	ClosedAt  *time.Time    `json:"closed_at,omitempty"`
	// LatestProgress is a snapshot of the last reconcile/close computation; the
	// live aggregate is always recomputed on read and is never authoritative.
	LatestProgress *CampaignProgress `json:"latest_progress,omitempty"`
	ReconciledAt   *time.Time        `json:"reconciled_at,omitempty"`
}

// CampaignMembership records a plan's position and campaign-scope lifecycle.
// It is keyed by campaign ID and plan ID and never mirrors mutable plan data.
type CampaignMembership struct {
	CampaignID  string              `json:"campaign_id"`
	PlanID      string              `json:"plan_id"`
	Position    int                 `json:"position"`
	State       CampaignMemberState `json:"state"`
	JoinedAt    time.Time           `json:"joined_at"`
	AbortedAt   *time.Time          `json:"aborted_at,omitempty"`
	AbortReason string              `json:"abort_reason,omitempty"`
}

// CampaignMember is a read-only, position-ordered view of one linked plan.
type CampaignMember struct {
	Position        int                 `json:"position"`
	PlanID          string              `json:"plan_id"`
	Zone            string              `json:"zone"`
	MembershipState CampaignMemberState `json:"membership_state"`
	PlanState       PlanState           `json:"plan_state"`
	LatestVerdict   Verdict             `json:"latest_verdict,omitempty"`
	NeedsReview     bool                `json:"needs_review"`
	// BlockingReason is empty for aborted or completed members.
	BlockingReason string     `json:"blocking_reason,omitempty"`
	AbortReason    string     `json:"abort_reason,omitempty"`
	JoinedAt       time.Time  `json:"joined_at"`
	AbortedAt      *time.Time `json:"aborted_at,omitempty"`
}

// CampaignBlocker explains why one non-aborted member keeps the campaign open.
type CampaignBlocker struct {
	Position int    `json:"position"`
	PlanID   string `json:"plan_id"`
	Code     string `json:"code"`
}

type CampaignProgress struct {
	TotalPlans      int               `json:"total_plans"`
	CompletedPlans  int               `json:"completed_plans"`
	AbortedPlans    int               `json:"aborted_plans"`
	PendingPlans    int               `json:"pending_plans"`
	CompletionRatio float64           `json:"completion_ratio"`
	ReadyToClose    bool              `json:"ready_to_close"`
	BlockingReasons []CampaignBlocker `json:"blocking_reasons"`
	ComputedAt      time.Time         `json:"computed_at"`
}

// CampaignView is the read model returned by every campaign endpoint. Progress
// is derived purely from current plan state; the campaign never writes plans.
type CampaignView struct {
	ReleaseCampaign
	Members  []CampaignMember `json:"members"`
	Progress CampaignProgress `json:"progress"`
}

type CampaignPage struct {
	Items []ReleaseCampaign `json:"items"`
	Meta  PageMeta          `json:"meta"`
}

func NormalizeCampaignInput(input CampaignCreateInput) CampaignCreateInput {
	input.Name = strings.TrimSpace(input.Name)
	input.Owner = strings.TrimSpace(input.Owner)
	return input
}

func ValidateCampaignInput(input CampaignCreateInput) error {
	input = NormalizeCampaignInput(input)
	details := make(map[string]any)
	if count := utf8.RuneCountInString(input.Name); count < 1 || count > MaxCampaignNameRunes {
		details["name"] = "must contain between 1 and 120 characters"
	} else if containsControlRune(input.Name) {
		details["name"] = "must not contain control characters"
	}
	if count := utf8.RuneCountInString(input.Owner); count < 2 || count > MaxCampaignOwnerRunes {
		details["owner"] = "must contain between 2 and 120 characters"
	} else if !validOwnerToken(input.Owner) {
		details["owner"] = "may contain only letters, digits, dots, hyphens, underscores, and @"
	}
	if len(details) > 0 {
		return Validation("campaign input is invalid", details)
	}
	return nil
}

func ValidateCampaignJoinInput(input CampaignJoinPlansInput) error {
	details := make(map[string]any)
	if len(input.PlanIDs) < 1 {
		details["plan_ids"] = "must contain at least one plan"
	} else if len(input.PlanIDs) > MaxCampaignPlanMembers {
		details["plan_ids"] = "must contain at most 100 plans"
	}
	for _, planID := range input.PlanIDs {
		if strings.TrimSpace(planID) == "" {
			details["plan_ids"] = "must not contain empty plan identifiers"
			break
		}
		if utf8.RuneCountInString(planID) > MaxNodeNameRunes {
			details["plan_ids"] = "contains a plan identifier that is too long"
			break
		}
	}
	if len(details) > 0 {
		return Validation("campaign plan input is invalid", details)
	}
	return nil
}

func ValidateCampaignReorderInput(input CampaignReorderInput) error {
	if len(input.PlanOrder) < 1 || len(input.PlanOrder) > MaxCampaignPlanMembers {
		return Validation("campaign order input is invalid", map[string]any{
			"plan_order": "must contain between 1 and 100 plans",
		})
	}
	for _, planID := range input.PlanOrder {
		if strings.TrimSpace(planID) == "" || utf8.RuneCountInString(planID) > MaxNodeNameRunes {
			return Validation("campaign order input is invalid", map[string]any{
				"plan_order": "contains an invalid plan identifier",
			})
		}
	}
	return nil
}

func ValidateCampaignAbortInput(input CampaignAbortPlanInput) error {
	input.Reason = strings.TrimSpace(input.Reason)
	count := utf8.RuneCountInString(input.Reason)
	if count < 1 || count > MaxCampaignAbortRunes || containsControlRune(input.Reason) {
		return Validation("campaign abort input is invalid", map[string]any{
			"reason": "must contain between 1 and 300 characters without control characters",
		})
	}
	return nil
}

// CampaignMembershipKey is the stable composite key for membership records.
func CampaignMembershipKey(campaignID, planID string) string {
	return campaignID + "\x00" + planID
}

// ValidatePlanOrder deterministically rejects malformed orders: a different
// member set, an unknown plan, or a repeated position. Ordering is a total
// order, so a cyclic order cannot be represented; any attempt to build one
// collapses into a duplicate or unknown identifier and is rejected here.
func ValidatePlanOrder(current []string, requested []string) error {
	if len(requested) != len(current) {
		return NewError(CodeInvalidPlanOrder, "plan order must contain exactly the current members", map[string]any{
			"expected_count": len(current),
			"actual_count":   len(requested),
		})
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, planID := range current {
		currentSet[planID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	for _, planID := range requested {
		if _, ok := currentSet[planID]; !ok {
			return NewError(CodeInvalidPlanOrder, "plan order references a plan that is not linked to the campaign", map[string]any{
				"plan_id": planID,
			})
		}
		if _, duplicated := seen[planID]; duplicated {
			return NewError(CodeInvalidPlanOrder, "plan order must not repeat a plan", map[string]any{
				"plan_id": planID,
			})
		}
		seen[planID] = struct{}{}
	}
	return nil
}

// AbortCampaignMembership applies the explicit active -> aborted transition.
func AbortCampaignMembership(membership *CampaignMembership, reason string, now time.Time) error {
	if membership.State == MemberAborted {
		return InvalidState("campaign membership is already aborted", map[string]any{
			"campaign_id": membership.CampaignID,
			"plan_id":     membership.PlanID,
		})
	}
	membership.State = MemberAborted
	membership.AbortReason = reason
	abortedAt := now.UTC()
	membership.AbortedAt = &abortedAt
	return nil
}

// CloseCampaign applies the explicit open -> closed transition.
func CloseCampaign(campaign *ReleaseCampaign, now time.Time) error {
	if campaign.State != CampaignOpen {
		return InvalidState("campaign can be closed only while open", map[string]any{
			"campaign_id": campaign.ID,
			"state":       campaign.State,
		})
	}
	campaign.State = CampaignClosed
	closedAt := now.UTC()
	campaign.ClosedAt = &closedAt
	campaign.Version++
	return nil
}

// BuildCampaignView derives the read-only aggregate. It never mutates state:
// blockers come only from active (non-aborted) members whose plans have not
// reached the plan's own completed state.
func BuildCampaignView(state *State, campaign ReleaseCampaign, now time.Time) CampaignView {
	members := make([]CampaignMember, 0, len(campaign.PlanIDs))
	blockers := make([]CampaignBlocker, 0)
	completed := 0
	aborted := 0
	pending := 0

	for position, planID := range campaign.PlanIDs {
		member := CampaignMember{
			Position: position,
			PlanID:   planID,
		}
		membership, hasMembership := state.CampaignMembers[CampaignMembershipKey(campaign.ID, planID)]
		if hasMembership {
			member.MembershipState = membership.State
			member.JoinedAt = membership.JoinedAt
			member.AbortedAt = membership.AbortedAt
			member.AbortReason = membership.AbortReason
		} else {
			member.MembershipState = MemberActive
			member.JoinedAt = campaign.OpenedAt
		}

		plan, hasPlan := state.Plans[planID]
		if hasPlan {
			member.Zone = plan.Zone
			member.PlanState = plan.State
			if assessment, assessed := state.Assessments[plan.LatestAssessmentID]; assessed {
				member.LatestVerdict = assessment.Verdict
				member.NeedsReview = assessment.Verdict == VerdictReviewRequired
			}
		}

		switch {
		case member.MembershipState == MemberAborted:
			aborted++
		case !hasPlan:
			member.BlockingReason = CampaignBlockerPlanNotFound
			pending++
			blockers = append(blockers, CampaignBlocker{
				Position: position,
				PlanID:   planID,
				Code:     CampaignBlockerPlanNotFound,
			})
		case plan.State == PlanCompleted:
			completed++
		case plan.State == PlanDraft:
			member.BlockingReason = CampaignBlockerPlanInDraft
			pending++
			blockers = append(blockers, CampaignBlocker{
				Position: position,
				PlanID:   planID,
				Code:     CampaignBlockerPlanInDraft,
			})
		default:
			member.BlockingReason = CampaignBlockerPlanInActive
			pending++
			blockers = append(blockers, CampaignBlocker{
				Position: position,
				PlanID:   planID,
				Code:     CampaignBlockerPlanInActive,
			})
		}
		members = append(members, member)
	}

	sort.SliceStable(blockers, func(i, j int) bool {
		return blockers[i].Position < blockers[j].Position
	})
	denominator := len(campaign.PlanIDs) - aborted
	ratio := 0.0
	if denominator > 0 {
		ratio = round(float64(completed)/float64(denominator), 6)
	}
	return CampaignView{
		ReleaseCampaign: campaign,
		Members:         members,
		Progress: CampaignProgress{
			TotalPlans:      len(campaign.PlanIDs),
			CompletedPlans:  completed,
			AbortedPlans:    aborted,
			PendingPlans:    pending,
			CompletionRatio: ratio,
			ReadyToClose:    len(blockers) == 0,
			BlockingReasons: blockers,
			ComputedAt:      now.UTC(),
		},
	}
}

func validOwnerToken(value string) bool {
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' || r == '@' {
			continue
		}
		return false
	}
	return true
}

func containsControlRune(value string) bool {
	for _, r := range value {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
