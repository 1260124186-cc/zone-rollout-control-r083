package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	var input domain.CampaignCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	campaign, err := s.campaigns.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, campaign)
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
	state := domain.CampaignState(strings.TrimSpace(r.URL.Query().Get("state")))
	if state != "" && state != domain.CampaignOpen && state != domain.CampaignClosed {
		sendError(w, r, domain.Validation("campaign state is not supported", map[string]any{"state": state}))
		return
	}
	page, err := s.campaigns.List(r.Context(), state, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	campaign, err := s.campaigns.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, campaign)
}

func (s *Server) addCampaignPlans(w http.ResponseWriter, r *http.Request) {
	var input domain.CampaignPlansInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	campaign, err := s.campaigns.AddPlans(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, campaign)
}

func (s *Server) campaignProgress(w http.ResponseWriter, r *http.Request) {
	progress, err := s.campaigns.Progress(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, progress)
}

func (s *Server) closeCampaign(w http.ResponseWriter, r *http.Request) {
	var input versionInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	if input.ExpectedVersion == 0 {
		sendError(w, r, domain.Validation("expected_version must be greater than zero", nil))
		return
	}
	campaign, err := s.campaigns.Close(r.Context(), r.PathValue("id"), input.ExpectedVersion)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, campaign)
}
