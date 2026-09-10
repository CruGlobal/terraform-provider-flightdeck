package flightdecktest

import (
	"net/http"
	"sort"
	"strings"
)

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/workspace-members", s.listWorkspaceMembers)
	})
}

// Workspace member kinds.
const (
	KindHuman   = "human"
	KindService = "service"
)

// AddMember seeds a person in the workspace and returns them.
func (s *Server) AddMember(name, email string) User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := User{ID: s.id(), Name: name, Email: email, Role: "member", Kind: KindHuman}
	s.members = append(s.members, u)
	return u
}

// AddServiceAccount seeds a service account (a bot) in the workspace and
// returns it. Service accounts are visible in the directory only to a
// workspace admin.
func (s *Server) AddServiceAccount(name, email string) User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := User{ID: s.id(), Name: name, Email: email, Role: "member", Kind: KindService}
	s.members = append(s.members, u)
	return u
}

// kindOf defaults an unset kind to human, so seeded users need not spell it.
func kindOf(u User) string {
	if u.Kind == "" {
		return KindHuman
	}
	return u.Kind
}

// normalizeEmail mirrors the model's normalization of the stored column, which
// is what makes the filter case- and whitespace-insensitive.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// listWorkspaceMembers is the directory: ordered by name, filtered by an EXACT
// email match, and split by role — humans are visible to any member, service
// accounts only to a workspace admin, which is why a non-admin filtering for a
// bot's address gets an empty list rather than a 403.
func (s *Server) listWorkspaceMembers(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filter := r.URL.Query().Get("email")
	users := append([]User(nil), s.members...)
	sort.Slice(users, func(i, j int) bool {
		if users[i].Name != users[j].Name {
			return users[i].Name < users[j].Name
		}
		return users[i].ID < users[j].ID
	})
	items := []any{}
	for _, u := range users {
		if kindOf(u) == KindService && !s.workspaceAdmin {
			continue
		}
		if filter != "" && normalizeEmail(u.Email) != normalizeEmail(filter) {
			continue
		}
		items = append(items, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "kind": kindOf(u)})
	}
	writeCollection(w, r, items)
}
