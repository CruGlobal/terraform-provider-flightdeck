package flightdecktest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The service-account routes, /api/v1/service-accounts and their api-tokens.
//
// A service account is a service-kind user joined to the workspace at the
// member role. In this fake, the workspace's members (Server.members) say
// which accounts are in THIS workspace, and serviceAccountStore.users holds
// every service account Flightdeck knows, in any workspace, so that one
// removed from here, or one that only another workspace has, can be added
// (back) with the same id and the name it already has. The user itself is
// never deleted.

// ServerZone is the zone the fake writes its times in, as the API writes its
// own: an ISO 8601 offset, not necessarily Z.
var ServerZone = time.FixedZone("EDT", -4*60*60)

// isoZoned is a time the way the API writes it: whole seconds, in its zone.
func isoZoned(t time.Time) string { return t.In(ServerZone).Format(time.RFC3339) }

// serviceUser is a service account anywhere in Flightdeck.
type serviceUser struct {
	ID    int64
	Name  string
	Email string
	// OtherWorkspaces counts the workspaces other than this one the account
	// is in. While it is above zero, the name is shared and can't be changed
	// here.
	OtherWorkspaces int
}

// ServiceAccountAPIToken is the fake's stored service-account token;
// Plaintext is kept only so the original create response can return it once.
type ServiceAccountAPIToken struct {
	ID        int64
	UserID    int64
	Name      string
	ExpiresAt *time.Time
	Plaintext string
	LastFour  string
	CreatedAt time.Time
}

type serviceAccountStore struct {
	users    map[int64]*serviceUser
	joinedAt map[int64]time.Time
	// people are the addresses of people outside this workspace. A person in
	// it is one of Server.members.
	people map[string]bool
	tokens map[int64]*ServiceAccountAPIToken
}

const (
	serviceAccountNameMax  = 255
	serviceAccountEmailMax = 255
	adminRefusal           = "This action requires workspace owner or admin rights."
)

// serviceEmailFormat is the model's own rule: one @, and no space or @ on
// either side of it. Ruby's \s is ASCII white space.
var serviceEmailFormat = regexp.MustCompile(`^[^@ \t\r\n\f\v]+@[^@ \t\r\n\f\v]+$`)

var serviceAccountReadOnly = map[string]string{
	"id":        "the server assigns it on create, and on update it is the :id in the path",
	"role":      "a service account joins the workspace as a plain member. Give it access to a project with POST /api/v1/projects/:project_id/members",
	"joined_at": "the server stamps it when the service account joins this workspace",
}

var serviceAccountTokenReadOnly = map[string]string{
	"id":                 "the server assigns it on create",
	"service_account_id": "it is the :service_account_id in the path. A token belongs to the service account it was made for and can't be moved",
	"masked":             "it is made from the secret for display; it is not stored and can't be set",
	"last_four":          "it is made from the secret the server mints",
	"last_used_at":       "the server stamps it when the token is used",
	"created_at":         "the server stamps it",
	"token":              "the server mints the secret and keeps only a SHA-256 digest of it. It is sent once, by the create that made it, and can never be set or read again",
	"secret_available":   "it is not a field of a token at all. It appears only on an Idempotency-Key REPLAY, to say the one-time secret is missing from that body rather than null",
	"message":            "it is not a field of a token at all. It is the text an Idempotency-Key REPLAY carries to say why the secret is not in it",
}

func init() {
	registerResource(func(s *Server, mux *http.ServeMux) {
		st := &serviceAccountStore{
			users: map[int64]*serviceUser{}, joinedAt: map[int64]time.Time{},
			people: map[string]bool{}, tokens: map[int64]*ServiceAccountAPIToken{},
		}
		s.stores["service_accounts"] = st
		mux.HandleFunc("GET /api/v1/service-accounts", s.listServiceAccounts)
		mux.HandleFunc("POST /api/v1/service-accounts", s.createServiceAccount)
		mux.HandleFunc("GET /api/v1/service-accounts/{id}", s.showServiceAccount)
		mux.HandleFunc("PATCH /api/v1/service-accounts/{id}", s.updateServiceAccount)
		mux.HandleFunc("DELETE /api/v1/service-accounts/{id}", s.destroyServiceAccount)
		mux.HandleFunc("GET /api/v1/service-accounts/{id}/api-tokens", s.listServiceAccountAPITokens)
		mux.HandleFunc("POST /api/v1/service-accounts/{id}/api-tokens", s.createServiceAccountAPIToken)
		mux.HandleFunc("GET /api/v1/service-accounts/{id}/api-tokens/{token_id}", s.showServiceAccountAPIToken)
		mux.HandleFunc("DELETE /api/v1/service-accounts/{id}/api-tokens/{token_id}", s.destroyServiceAccountAPIToken)
	})
}

func (s *Server) serviceAccounts() *serviceAccountStore {
	store, _ := s.stores["service_accounts"].(*serviceAccountStore)
	return store
}

// --- test seams ----------------------------------------------------------------

// AddServiceAccountElsewhere makes a service account Flightdeck knows but that
// is not in this workspace, and returns its id. With inAnotherWorkspace it is
// in some other workspace, so its name is shared and a create here keeps it;
// without, it is in none (it was removed from every workspace it was in),
// and a create here gives it the name asked for.
func (s *Server) AddServiceAccountElsewhere(name, email string, inAnotherWorkspace bool) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &serviceUser{ID: s.id(), Name: name, Email: normalizeEmail(email)}
	if inAnotherWorkspace {
		u.OtherWorkspaces = 1
	}
	s.serviceAccounts().users[u.ID] = u
	return u.ID
}

// AddPersonElsewhere records a person outside this workspace, whose address a
// service account can't take.
func (s *Server) AddPersonElsewhere(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serviceAccounts().people[normalizeEmail(email)] = true
}

// JoinServiceAccountToAnotherWorkspace puts a service account of this
// workspace in another one too, which makes its name shared.
func (s *Server) JoinServiceAccountToAnotherWorkspace(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.serviceUserLocked(id); u != nil {
		u.OtherWorkspaces++
	}
}

// ServiceAccountInWorkspace reports whether a service account is in this
// workspace, and its name there.
func (s *Server) ServiceAccountInWorkspace(id int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.serviceMemberLocked(id); m != nil {
		return m.Name, true
	}
	return "", false
}

// RemoveServiceAccountOutOfBand removes a service account from this
// workspace the way the Service accounts page does: its tokens and project
// memberships here go with it, and the user stays.
func (s *Server) RemoveServiceAccountOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeServiceAccountLocked(id)
}

// RenameServiceAccountOutOfBand renames a service account the way the page
// does.
func (s *Server) RenameServiceAccountOutOfBand(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renameServiceAccountLocked(id, name)
}

// ServiceAccountAPIToken returns a copy of a stored token, or nil when it was
// deleted.
func (s *Server) ServiceAccountAPIToken(id int64) *ServiceAccountAPIToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.serviceAccounts().tokens[id]; t != nil {
		out := *t
		return &out
	}
	return nil
}

// ServiceAccountAPITokenIDs returns the ids of a service account's tokens,
// sorted.
func (s *Server) ServiceAccountAPITokenIDs(userID int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for _, t := range s.serviceAccounts().tokens {
		if t.UserID == userID {
			ids = append(ids, t.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ExpireServiceAccountAPITokenOutOfBand moves a token's expiry to a moment
// ago, as if its time had come: the fake's stand-in for waiting.
func (s *Server) ExpireServiceAccountAPITokenOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.serviceAccounts().tokens[id]; t != nil {
		past := time.Now().Add(-time.Second)
		t.ExpiresAt = &past
	}
}

// DeleteServiceAccountAPITokenOutOfBand deletes a token the way the page's
// revoke does.
func (s *Server) DeleteServiceAccountAPITokenOutOfBand(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.serviceAccounts().tokens, id)
}

// --- store helpers (called with s.mu held) ---------------------------------------

// serviceMemberLocked is the workspace member that is service account id, or
// nil when no service account with that id is in this workspace.
func (s *Server) serviceMemberLocked(id int64) *User {
	for i := range s.members {
		if s.members[i].ID == id && kindOf(s.members[i]) == KindService {
			return &s.members[i]
		}
	}
	return nil
}

// serviceUserLocked is the service account with this id anywhere, or nil.
// A service account seeded straight into the workspace's members is
// registered here the first time it is asked for.
func (s *Server) serviceUserLocked(id int64) *serviceUser {
	st := s.serviceAccounts()
	if u := st.users[id]; u != nil {
		return u
	}
	if m := s.serviceMemberLocked(id); m != nil {
		u := &serviceUser{ID: m.ID, Name: m.Name, Email: normalizeEmail(m.Email)}
		st.users[id] = u
		return u
	}
	return nil
}

// joinedAtLocked is when a service account joined this workspace. One seeded
// straight into the members joined when the fake started.
func (s *Server) joinedAtLocked(id int64) time.Time {
	st := s.serviceAccounts()
	at, ok := st.joinedAt[id]
	if !ok {
		at = time.Now()
		st.joinedAt[id] = at
	}
	return at
}

func (s *Server) serializeServiceAccountLocked(m *User) map[string]any {
	return map[string]any{
		"id": m.ID, "name": m.Name, "email_address": normalizeEmail(m.Email),
		"role": "member", "joined_at": isoZoned(s.joinedAtLocked(m.ID)),
	}
}

func (s *Server) renameServiceAccountLocked(id int64, name string) {
	if u := s.serviceUserLocked(id); u != nil {
		u.Name = name
	}
	if m := s.serviceMemberLocked(id); m != nil {
		m.Name = name
	}
}

// removeServiceAccountLocked takes a service account out of this workspace:
// its tokens and project memberships here are deleted, and the user stays.
func (s *Server) removeServiceAccountLocked(id int64) {
	s.serviceUserLocked(id) // keep the user, with its name, for a later join
	for i, m := range s.members {
		if m.ID == id && kindOf(m) == KindService {
			s.members = append(s.members[:i], s.members[i+1:]...)
			break
		}
	}
	st := s.serviceAccounts()
	delete(st.joinedAt, id)
	for tid, t := range st.tokens {
		if t.UserID == id {
			delete(st.tokens, tid)
		}
	}
	for mid, m := range s.projectMembers().byID {
		if m.UserID == id {
			delete(s.projectMembers().byID, mid)
		}
	}
}

// adminOnly is the gate every service-account action asks first, ahead of
// any lookup, so a caller below it learns nothing about which ids exist.
func (s *Server) adminOnly(w http.ResponseWriter) bool {
	s.mu.Lock()
	admin := s.workspaceAdmin
	s.mu.Unlock()
	if !admin {
		writeError(w, http.StatusForbidden, "forbidden", adminRefusal)
		return false
	}
	return true
}

// rubyClass names a decoded JSON value's type the way the API's messages do.
func rubyClass(v any) string {
	switch t := v.(type) {
	case nil:
		return "NilClass"
	case string:
		return "String"
	case bool:
		if t {
			return "TrueClass"
		}
		return "FalseClass"
	case json.Number:
		if strings.ContainsAny(t.String(), ".eE") {
			return "Float"
		}
		return "Integer"
	case []any:
		return "Array"
	case map[string]any:
		return "Hash"
	}
	return fmt.Sprintf("%T", v)
}

// refuseKeys refuses read-only keys by name, then unknown ones, in the
// API's order. It returns the refusal, or "".
func refuseKeys(attrs map[string]any, readOnly map[string]string, writable []string) string {
	var named []string
	for key := range readOnly {
		if _, has := attrs[key]; has {
			named = append(named, key)
		}
	}
	if len(named) > 0 {
		sort.Strings(named)
		parts := make([]string, 0, len(named))
		for _, key := range named {
			parts = append(parts, key+" is read-only over the API — "+readOnly[key])
		}
		return strings.Join(parts, "; ")
	}
	var unknown []string
	for key := range attrs {
		if !contains(writable, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "unknown key: " + strings.Join(unknown, ", ") + " (settable: " + strings.Join(writable, ", ") + ")"
	}
	return ""
}

// presentName is Ruby's `strip.presence`: the string with ASCII blank space
// cut from either end, or "" when nothing but (Unicode) blank space is left.
func presentName(s string) string {
	name := strings.Trim(s, apiBlank)
	if strings.TrimFunc(name, unicode.IsSpace) == "" {
		return ""
	}
	return name
}

// coerceServiceAccountCreate is the create's reading of its body: the
// address (stripped, lowercased, shaped like one) and the name ("" for
// none). It returns a 422 message naming the field, or "".
func coerceServiceAccountCreate(attrs map[string]any) (email, name, refusal string) {
	if refusal := refuseKeys(attrs, serviceAccountReadOnly, []string{"email_address", "name"}); refusal != "" {
		return "", "", refusal
	}
	raw := attrs["email_address"]
	if blankValue(raw) {
		return "", "", "email_address is required"
	}
	str, ok := raw.(string)
	if !ok {
		return "", "", "email_address must be a string, got " + rubyClass(raw)
	}
	email = strings.ToLower(strings.Trim(str, apiBlank))
	if utf8.RuneCountInString(email) > serviceAccountEmailMax {
		return "", "", fmt.Sprintf("email_address must be %d characters or fewer", serviceAccountEmailMax)
	}
	if !serviceEmailFormat.MatchString(email) {
		return "", "", fmt.Sprintf("email_address must look like name@example.com, got %q", str)
	}
	if v := attrs["name"]; !blankValue(v) {
		str, ok := v.(string)
		if !ok {
			return "", "", "name must be a string, got " + rubyClass(v)
		}
		name = presentName(str)
		if utf8.RuneCountInString(name) > serviceAccountNameMax {
			return "", "", fmt.Sprintf("name must be %d characters or fewer", serviceAccountNameMax)
		}
	}
	return email, name, ""
}

// --- service accounts ----------------------------------------------------------

func (s *Server) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w) {
		return
	}
	s.mu.Lock()
	var rows []*User
	for i := range s.members {
		if kindOf(s.members[i]) == KindService {
			rows = append(rows, &s.members[i])
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	items := make([]any, 0, len(rows))
	for _, m := range rows {
		items = append(items, s.serializeServiceAccountLocked(m))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.serviceMemberLocked(id)
	if m == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.serializeServiceAccountLocked(m))
}

// createServiceAccount makes a service account here, or adds one Flightdeck
// already has: one still in another workspace keeps its (shared) name, and
// one in no workspace takes the name asked for. The create is fingerprinted
// with a blank name left out, so the same key with another address is
// refused.
func (s *Server) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w) {
		return
	}
	attrs, ok := decodeTeamspaceBody(w, r, "service_account")
	if !ok {
		return
	}
	fingerprinted := map[string]any{}
	for k, v := range attrs {
		if k == "name" && blankValue(v) {
			continue
		}
		fingerprinted[k] = v
	}
	s.withIdempotencyFingerprint(w, r, "service_account", fingerprintOf(fingerprinted), func() (int, any) {
		email, name, refusal := coerceServiceAccountCreate(attrs)
		if refusal != "" {
			return http.StatusUnprocessableEntity, errorBody(refusal, "invalid_attribute")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.serviceAccounts()
		for _, m := range s.members {
			if normalizeEmail(m.Email) != email {
				continue
			}
			if kindOf(m) != KindService {
				return http.StatusUnprocessableEntity, errorBody(
					fmt.Sprintf("email_address %s belongs to a person, not a service account", email), "invalid_attribute")
			}
			return http.StatusUnprocessableEntity, errorBody(
				fmt.Sprintf("email_address %s is already a service account in this workspace", email), "validation_failed")
		}
		if st.people[email] {
			return http.StatusUnprocessableEntity, errorBody(
				fmt.Sprintf("email_address %s belongs to a person, not a service account", email), "invalid_attribute")
		}
		var user *serviceUser
		for _, u := range st.users {
			if u.Email == email {
				user = u
				break
			}
		}
		switch {
		case user == nil:
			if name == "" {
				name = email[:strings.Index(email, "@")]
			}
			user = &serviceUser{ID: s.id(), Name: name, Email: email}
			st.users[user.ID] = user
		case user.OtherWorkspaces == 0 && name != "":
			// An account in no workspace at all (one removed earlier; the
			// user is kept) takes the name asked for. A blank name keeps the
			// one it has.
			user.Name = name
		}
		// An account still in another workspace joins with the name it has,
		// which is shared there; the name in the body is not applied.
		s.members = append(s.members, User{ID: user.ID, Name: user.Name, Email: user.Email, Role: "member", Kind: KindService})
		st.joinedAt[user.ID] = time.Now()
		return http.StatusCreated, s.serializeServiceAccountLocked(s.serviceMemberLocked(user.ID))
	})
}

// updateServiceAccount renames a service account. The stored address (in
// any case) or a blank one is accepted back; any other address is refused.
// A rename is refused while the account is in another workspace too, unless
// the name is the one it already has.
func (s *Server) updateServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	found := s.serviceMemberLocked(id) != nil
	s.mu.Unlock()
	if !found {
		notFound(w)
		return
	}
	attrs, ok := decodeTeamspaceBody(w, r, "service_account")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.serviceMemberLocked(id)
	if m == nil {
		notFound(w)
		return
	}
	if refusal := refuseKeys(attrs, serviceAccountReadOnly, []string{"email_address", "name"}); refusal != "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", refusal)
		return
	}
	if v, has := attrs["email_address"]; has && !blankValue(v) {
		str, isStr := v.(string)
		if !isStr {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "email_address must be a string, got "+rubyClass(v))
			return
		}
		if strings.ToLower(strings.Trim(str, apiBlank)) != normalizeEmail(m.Email) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
				"email_address can't be changed. To use a new address, create a new service account and remove this one")
			return
		}
	}
	if v, has := attrs["name"]; has && !blankValue(v) {
		str, isStr := v.(string)
		if !isStr {
			writeError(w, http.StatusUnprocessableEntity, "invalid_attribute", "name must be a string, got "+rubyClass(v))
			return
		}
		if name := presentName(str); name != "" && name != m.Name {
			if utf8.RuneCountInString(name) > serviceAccountNameMax {
				writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
					fmt.Sprintf("name must be %d characters or fewer", serviceAccountNameMax))
				return
			}
			if u := s.serviceUserLocked(id); u != nil && u.OtherWorkspaces > 0 {
				writeError(w, http.StatusUnprocessableEntity, "invalid_attribute",
					"name can't be changed here because this service account is also in another workspace, "+
						"and its name is shared by every workspace it is in")
				return
			}
			s.renameServiceAccountLocked(id, name)
		}
	}
	writeJSON(w, http.StatusOK, s.serializeServiceAccountLocked(s.serviceMemberLocked(id)))
}

// destroyServiceAccount removes the account from this workspace only.
func (s *Server) destroyServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serviceMemberLocked(id) == nil {
		notFound(w)
		return
	}
	s.removeServiceAccountLocked(id)
	w.WriteHeader(http.StatusNoContent)
}

// --- service-account API tokens ----------------------------------------------------

// serviceAccountFromPath resolves the {id} segment to a service account in
// this workspace, or answers 404. It takes s.mu itself.
func (s *Server) serviceAccountFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !s.adminOnly(w) {
		return 0, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return 0, false
	}
	s.mu.Lock()
	found := s.serviceMemberLocked(id) != nil
	s.mu.Unlock()
	if !found {
		notFound(w)
		return 0, false
	}
	return id, true
}

func (t *ServiceAccountAPIToken) expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

func serializeServiceAccountAPIToken(t *ServiceAccountAPIToken, reveal bool) map[string]any {
	out := map[string]any{
		"id": t.ID, "service_account_id": t.UserID, "name": t.Name,
		"masked": "fd_pat_…" + t.LastFour, "last_four": t.LastFour,
		"expires_at": nil, "last_used_at": nil, "created_at": isoZoned(t.CreatedAt),
	}
	if t.ExpiresAt != nil {
		out["expires_at"] = isoZoned(*t.ExpiresAt)
	}
	if reveal {
		out["token"] = t.Plaintext
	}
	return out
}

// liveServiceAccountTokenLocked is token id of service account userID, while
// the account is in this workspace, or nil.
func (s *Server) liveServiceAccountTokenLocked(userID, id int64) *ServiceAccountAPIToken {
	t := s.serviceAccounts().tokens[id]
	if t == nil || t.UserID != userID || s.serviceMemberLocked(userID) == nil {
		return nil
	}
	return t
}

func (s *Server) listServiceAccountAPITokens(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.serviceAccountFromPath(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	var rows []*ServiceAccountAPIToken
	for _, t := range s.serviceAccounts().tokens {
		if t.UserID == userID {
			rows = append(rows, t)
		}
	}
	// Newest first, with the id as the tiebreak.
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	items := make([]any, 0, len(rows))
	for _, t := range rows {
		items = append(items, serializeServiceAccountAPIToken(t, false))
	}
	s.mu.Unlock()
	writeCollection(w, r, items)
}

func (s *Server) showServiceAccountAPIToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.serviceAccountFromPath(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "token_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.liveServiceAccountTokenLocked(userID, id)
	if t == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, serializeServiceAccountAPIToken(t, false))
}

var (
	isoDatePattern       = regexp.MustCompile(`^\d{4}-\d{1,2}-\d{1,2}$`)
	isoNoOffsetTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?$`)
)

// parseExpiresAt reads expires_at the way the API does: an ISO 8601
// timestamp with an offset, one without (read in the server's zone), or a
// date alone (midnight in that zone). It returns a refusal, or "".
func parseExpiresAt(raw any) (*time.Time, string) {
	if blankValue(raw) {
		return nil, ""
	}
	str, ok := raw.(string)
	refusal := fmt.Sprintf("expires_at must be an ISO 8601 timestamp (e.g. 2026-09-23T14:30:00Z) or date (YYYY-MM-DD), got %s",
		func() string { b, _ := json.Marshal(raw); return string(b) }())
	if !ok {
		return nil, refusal
	}
	value := strings.Trim(str, apiBlank)
	var (
		at  time.Time
		err error
	)
	switch {
	case isoDatePattern.MatchString(value):
		at, err = time.ParseInLocation("2006-1-2", value, ServerZone)
	case isoNoOffsetTimestamp.MatchString(value):
		layout := "2006-01-02T15:04"
		if strings.Count(value, ":") == 2 {
			layout = "2006-01-02T15:04:05"
		}
		at, err = time.ParseInLocation(layout, strings.Replace(value, " ", "T", 1), ServerZone)
	default:
		at, err = time.Parse(time.RFC3339, value)
		if err == nil {
			// Go takes an offset of up to +24:59 (and +23:60); Ruby's Time
			// refuses one of a day or more either way, and the API answers
			// that with its usual refusal.
			if _, offset := at.Zone(); offset >= 24*60*60 || offset <= -24*60*60 {
				return nil, refusal
			}
		}
	}
	if err != nil {
		return nil, refusal
	}
	return &at, ""
}

// createServiceAccountAPIToken mints a token and renders its secret once. The
// idempotency cache holds a REDACTED body, and while the token a replay
// names is live (not deleted, not expired), the replay is refused with 409
// idempotency_replay_withheld. Fingerprinted with a blank expires_at left out.
func (s *Server) createServiceAccountAPIToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.serviceAccountFromPath(w, r)
	if !ok {
		return
	}
	attrs, ok := decodeTeamspaceBody(w, r, "api_token")
	if !ok {
		return
	}
	fingerprinted := map[string]any{}
	for k, v := range attrs {
		if k == "expires_at" && blankValue(v) {
			continue
		}
		fingerprinted[k] = v
	}
	withheld := &withheldSecret{
		live: func(replayed map[string]any) bool {
			id, _ := asInt64(replayed["id"])
			t := s.liveServiceAccountTokenLocked(userID, id)
			return t != nil && !t.expired(time.Now())
		},
		message: func(id int64) string {
			return fmt.Sprintf("This create was already done, and the token it made is still live, so its secret "+
				"can't be sent again. If this is a retry of your own create, delete token %d and "+
				"create again. If this service account needs another token with the same name and "+
				"expiry, change one of them.", id)
		},
	}
	s.idempotentlyWithheld(w, r, "service_account_api_token", fingerprintOf(fingerprinted), withheld, func() (int, any, any) {
		if refusal := refuseKeys(attrs, serviceAccountTokenReadOnly, []string{"name", "expires_at"}); refusal != "" {
			return http.StatusUnprocessableEntity, errorBody(refusal, "invalid_attribute"), nil
		}
		rawName := attrs["name"]
		if blankValue(rawName) {
			return http.StatusUnprocessableEntity, errorBody("name is required", "invalid_attribute"), nil
		}
		nameStr, isStr := rawName.(string)
		if !isStr {
			return http.StatusUnprocessableEntity, errorBody("name must be a string, got "+rubyClass(rawName), "invalid_attribute"), nil
		}
		name := presentName(nameStr)
		if name == "" {
			return http.StatusUnprocessableEntity, errorBody("name is required", "invalid_attribute"), nil
		}
		if utf8.RuneCountInString(name) > serviceAccountNameMax {
			return http.StatusUnprocessableEntity, errorBody(
				fmt.Sprintf("name must be %d characters or fewer", serviceAccountNameMax), "invalid_attribute"), nil
		}
		expiresAt, refusal := parseExpiresAt(attrs["expires_at"])
		if refusal != "" {
			return http.StatusUnprocessableEntity, errorBody(refusal, "invalid_attribute"), nil
		}
		if expiresAt != nil && !expiresAt.After(time.Now()) {
			return http.StatusUnprocessableEntity, errorBody(
				"expires_at must be in the future, got "+expiresAt.UTC().Format(time.RFC3339), "invalid_attribute"), nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.serviceMemberLocked(userID) == nil {
			return http.StatusNotFound, errorBody("Not found", "not_found"), nil
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		t := &ServiceAccountAPIToken{ID: s.id(), UserID: userID, Name: name, ExpiresAt: expiresAt, CreatedAt: time.Now()}
		t.Plaintext = "fd_pat_" + base64.RawURLEncoding.EncodeToString(raw)
		t.LastFour = t.Plaintext[len(t.Plaintext)-4:]
		s.serviceAccounts().tokens[t.ID] = t
		redacted := serializeServiceAccountAPIToken(t, false)
		redacted["token"] = nil
		redacted["secret_available"] = false
		redacted["message"] = "Replay of a previously-used Idempotency-Key. The token is returned only by the original " +
			"create and is never stored, so it can't be replayed. This token has since been deleted or has expired; " +
			"create a new one if you need it."
		return http.StatusCreated, serializeServiceAccountAPIToken(t, true), redacted
	})
}

func (s *Server) destroyServiceAccountAPIToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.serviceAccountFromPath(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "token_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveServiceAccountTokenLocked(userID, id) == nil {
		notFound(w)
		return
	}
	delete(s.serviceAccounts().tokens, id)
	w.WriteHeader(http.StatusNoContent)
}
