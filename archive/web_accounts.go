package archive

// HTTP surface for account management: list, add, switch, sign out, rename and
// remove. These endpoints are the only way to leave the signed-in state, so
// every mutation answers with the resulting list to keep the UI consistent
// without a second round trip.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"
)

// accountJSON is the wire shape. Namespace is deliberately omitted: it is an
// implementation detail, and exposing it would invite callers to address
// accounts by something the registry can rename out from under them.
type accountJSON struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AddedAt    time.Time `json:"added_at"`
	LastUsed   time.Time `json:"last_used,omitempty"`
	Legacy     bool      `json:"legacy,omitempty"`
	Active     bool      `json:"active"`
	HasSession bool      `json:"has_session"`
	State      string    `json:"state,omitempty"`
	Authorized bool      `json:"authorized,omitempty"`
	User       *WebUser  `json:"user,omitempty"`
}

type accountsAPIResponse struct {
	Accounts []accountJSON `json:"accounts"`
	Active   string        `json:"active"`
	// Managed is false when serve runs without account support at all, which
	// lets the UI hide the whole panel instead of showing an empty list that
	// looks like "no accounts exist".
	Managed bool `json:"managed"`
}

type accountMutation struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// accountsView builds the list payload. The active account contributes live
// controller state; inactive accounts can only report whether a session file
// exists, because no controller is running for them.
func (w *WebServer) accountsView(r *http.Request) accountsAPIResponse {
	if w.Accounts == nil {
		return accountsAPIResponse{Managed: false}
	}
	active := w.Accounts.ActiveID()
	current := w.Accounts.Current()
	out := accountsAPIResponse{Managed: true, Active: active, Accounts: []accountJSON{}}

	for _, a := range w.Accounts.List() {
		item := accountJSON{
			ID: a.ID, Name: a.Name, AddedAt: a.AddedAt, LastUsed: a.LastUsed,
			Legacy: a.Legacy, Active: a.ID == active,
		}
		if item.Active && current != nil && current.Ctl != nil {
			st := current.Ctl.State()
			item.State = st.State
			item.Authorized = st.Authorized
			item.User = st.User
			item.HasSession = st.Authorized
		} else {
			item.HasSession = w.Accounts.HasSession(r.Context(), a.ID)
		}
		out.Accounts = append(out.Accounts, item)
	}
	return out
}

func (w *WebServer) requireAccounts(rw http.ResponseWriter) bool {
	if w.Accounts == nil {
		http.Error(rw, "account management disabled: serve was started without a Telegram session store", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func decodeAccountBody(rw http.ResponseWriter, r *http.Request) (accountMutation, bool) {
	var body accountMutation
	if r.Body == nil {
		return body, true
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		// An empty body is valid for "add with a default name".
		if err.Error() == "EOF" {
			return body, true
		}
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// accountsAPI handles GET (list) and POST (create) on /api/accounts.
func (w *WebServer) accountsAPI(rw http.ResponseWriter, r *http.Request) {
	if !w.requireAccounts(rw) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(rw, w.accountsView(r))
	case http.MethodPost:
		body, ok := decodeAccountBody(rw, r)
		if !ok {
			return
		}
		a, err := w.Accounts.Add(r.Context(), body.Name)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		view := w.accountsView(r)
		rw.WriteHeader(http.StatusCreated)
		writeJSON(rw, map[string]any{
			"status":   "created",
			"id":       a.ID,
			"accounts": view.Accounts,
			"active":   view.Active,
		})
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// accountRoute handles /api/accounts/{id} and /api/accounts/{id}/{action}.
func (w *WebServer) accountRoute(rw http.ResponseWriter, r *http.Request) {
	if !w.requireAccounts(rw) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	if rest == "" {
		w.accountsAPI(rw, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodPost && action == "switch":
		if err := w.Accounts.Switch(r.Context(), id); err != nil {
			w.accountError(rw, err)
			return
		}
		w.writeAccountsResult(rw, r, "switched")

	case r.Method == http.MethodPost && action == "logout":
		if err := w.Accounts.Logout(r.Context(), id); err != nil {
			w.accountError(rw, err)
			return
		}
		w.writeAccountsResult(rw, r, "signed_out")

	case r.Method == http.MethodPost && action == "rename":
		body, ok := decodeAccountBody(rw, r)
		if !ok {
			return
		}
		if err := w.Accounts.Rename(r.Context(), id, body.Name); err != nil {
			w.accountError(rw, err)
			return
		}
		w.writeAccountsResult(rw, r, "renamed")

	case r.Method == http.MethodDelete && action == "":
		purge := false
		if v := r.URL.Query().Get("purge"); v != "" {
			purge, _ = strconv.ParseBool(v)
		}
		if err := w.Accounts.Remove(r.Context(), id, purge); err != nil {
			w.accountError(rw, err)
			return
		}
		w.writeAccountsResult(rw, r, "removed")

	case action == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		for _, a := range w.accountsView(r).Accounts {
			if a.ID == id {
				writeJSON(rw, a)
				return
			}
		}
		http.Error(rw, "account not found", http.StatusNotFound)

	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (w *WebServer) writeAccountsResult(rw http.ResponseWriter, r *http.Request, status string) {
	view := w.accountsView(r)
	writeJSON(rw, map[string]any{
		"status":   status,
		"accounts": view.Accounts,
		"active":   view.Active,
	})
}

// accountError maps a manager error onto an HTTP status. The manager returns
// sentinel errors for the two cases a client can actually cause, so this stays
// a lookup instead of string matching.
func (w *WebServer) accountError(rw http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAccountNotFound):
		http.Error(rw, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrAccountNameEmpty):
		http.Error(rw, err.Error(), http.StatusBadRequest)
	default:
		slog.Error("account operation failed", slog.Any("error", err))
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	}
}
