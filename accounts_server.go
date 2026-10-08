package main

import (
	"encoding/json"
	"errors"
	"net/http"
)

func writeAccountError(response http.ResponseWriter, err error) bool {
	status := 0
	switch {
	case errors.Is(err, errAccountNotFound):
		status = http.StatusNotFound
	case errors.Is(err, errAccountNotReady), errors.Is(err, errLastAccount), errors.Is(err, errPrimaryAccount):
		status = http.StatusConflict
	}
	if status == 0 {
		return false
	}
	writeJSON(response, status, map[string]string{"error": err.Error()})
	return true
}

func (s *Server) handleAccounts(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.usage.AccountsView())
}

func (s *Server) handleAccountsUsage(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}
	writeJSON(response, http.StatusOK, s.usage.GetAccountsUsage(request.Context(), force))
}

func (s *Server) handleAccountCreate(response http.ResponseWriter, request *http.Request) {
	s.saveAccount(response, request, "", http.StatusCreated)
}

func (s *Server) handleAccountUpdate(response http.ResponseWriter, request *http.Request) {
	s.saveAccount(response, request, request.PathValue("account_id"), http.StatusOK)
}

func (s *Server) saveAccount(response http.ResponseWriter, request *http.Request, id string, status int) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input AccountUpdate
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	view, err := s.usage.SaveAccount(id, input)
	if err != nil {
		if !writeAccountError(response, err) {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(response, status, view)
}

func (s *Server) handleAccountSwitch(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 4096)
	var input struct {
		AccountID string `json:"account_id"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.AccountID == "" {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "account_id is required"})
		return
	}
	view, err := s.usage.SwitchAccount(input.AccountID)
	if err != nil {
		if !writeAccountError(response, err) {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) handleAccountDelete(response http.ResponseWriter, request *http.Request) {
	view, err := s.usage.DeleteAccount(request.PathValue("account_id"))
	if err != nil {
		if !writeAccountError(response, err) {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(response, http.StatusOK, view)
}
