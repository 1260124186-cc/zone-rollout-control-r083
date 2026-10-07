package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

type campaignVersionInput struct {
	ExpectedVersion uint64 `json:"expected_version"`
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	var input domain.CampaignCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.campaigns.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, view)
}

func (s *Server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	stateFilter := domain.CampaignState(strings.TrimSpace(r.URL.Query().Get("state")))
	if stateFilter != "" && stateFilter != domain.CampaignOpen && stateFilter != domain.CampaignClosed {
		sendError(w, r, domain.Validation("campaign state is not supported", map[string]any{"state": stateFilter}))
		return
	}
	page, err := s.campaigns.List(r.Context(), stateFilter, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	view, err := s.campaigns.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) joinCampaignPlans(w http.ResponseWriter, r *http.Request) {
	var input domain.CampaignJoinPlansInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.campaigns.JoinPlans(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, view)
}

func (s *Server) reorderCampaignPlans(w http.ResponseWriter, r *http.Request) {
	var input domain.CampaignReorderInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.campaigns.Reorder(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) abortCampaignPlan(w http.ResponseWriter, r *http.Request) {
	var input domain.CampaignAbortPlanInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.campaigns.AbortPlan(r.Context(), r.PathValue("id"), r.PathValue("plan_id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) reconcileCampaign(w http.ResponseWriter, r *http.Request) {
	view, err := s.campaigns.Reconcile(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}

func (s *Server) closeCampaign(w http.ResponseWriter, r *http.Request) {
	var input campaignVersionInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	view, err := s.campaigns.Close(r.Context(), r.PathValue("id"), input.ExpectedVersion)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, view)
}
