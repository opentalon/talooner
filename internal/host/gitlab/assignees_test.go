package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

type assignServer struct {
	mu sync.Mutex

	assignees []participant
	reviewers []participant
	users     map[string]int

	putBodies  []map[string][]int
	mrStatus   int
	putStatus  int
	userStatus int
}

func (s *assignServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasPrefix(r.URL.Path, "/users"):
			if s.userStatus != 0 {
				w.WriteHeader(s.userStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			login := r.URL.Query().Get("username")
			id, ok := s.users[login]
			if !ok {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			_, _ = fmt.Fprintf(w, `[{"id":%d,"username":%s}]`, id, jsonString(login))

		case r.Method == http.MethodGet:
			if s.mrStatus != 0 {
				w.WriteHeader(s.mrStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			raw, err := json.Marshal(mrParticipantsPayload{Assignees: s.assignees, Reviewers: s.reviewers})
			if err != nil {
				t.Errorf("encode participants: %v", err)
			}
			_, _ = w.Write(raw)

		case r.Method == http.MethodPut:
			if s.putStatus != 0 {
				w.WriteHeader(s.putStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var body map[string][]int
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode put: %v", err)
			}
			s.putBodies = append(s.putBodies, body)

			byID := func(ps []participant) map[int]participant {
				out := map[int]participant{}
				for _, p := range ps {
					out[p.ID] = p
				}
				return out
			}
			all := byID(append(append([]participant{}, s.assignees...), s.reviewers...))
			for login, id := range s.users {
				all[id] = participant{ID: id, Username: login}
			}

			resolve := func(ids []int) []participant {
				out := make([]participant, 0, len(ids))
				for _, id := range ids {
					if p, ok := all[id]; ok {
						out = append(out, p)
					}
				}
				return out
			}

			updated := mrParticipantsPayload{Assignees: s.assignees, Reviewers: s.reviewers}
			if ids, ok := body["assignee_ids"]; ok {
				updated.Assignees = resolve(ids)
			}
			if ids, ok := body["reviewer_ids"]; ok {
				updated.Reviewers = resolve(ids)
			}
			raw, err := json.Marshal(updated)
			if err != nil {
				t.Errorf("encode updated participants: %v", err)
			}
			_, _ = w.Write(raw)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	c, _ := newTestClient(t, srv)
	t.Cleanup(srv.Close)
	return c
}

func sortedStrings(ss []string) []string {
	out := append([]string{}, ss...)
	sort.Strings(out)
	return out
}

func TestAddAssigneesUnionsWithCurrent(t *testing.T) {
	s := &assignServer{
		assignees: []participant{{ID: 1, Username: "alice"}},
		users:     map[string]int{"bob": 2},
	}
	c := s.client(t)

	got, err := c.AddAssignees(context.Background(), "o", "r", 5, []string{"bob"})
	if err != nil {
		t.Fatalf("AddAssignees: %v", err)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(sortedStrings(got), want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestRemoveAssigneesDropsMatchingID(t *testing.T) {
	s := &assignServer{
		assignees: []participant{{ID: 1, Username: "alice"}, {ID: 2, Username: "bob"}},
		users:     map[string]int{"bob": 2},
	}
	c := s.client(t)

	got, err := c.RemoveAssignees(context.Background(), "o", "r", 5, []string{"bob"})
	if err != nil {
		t.Fatalf("RemoveAssignees: %v", err)
	}
	want := []string{"alice"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestAddAssigneesSkipsUnknownLoginWithoutError(t *testing.T) {
	s := &assignServer{users: map[string]int{}}
	c := s.client(t)

	got, err := c.AddAssignees(context.Background(), "o", "r", 5, []string{"ghost"})
	if err != nil {
		t.Fatalf("AddAssignees: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty (unknown login skipped, not errored)", got)
	}
	if len(s.putBodies) != 1 || len(s.putBodies[0]["assignee_ids"]) != 0 {
		t.Errorf("putBodies = %v, want a put with an empty assignee_ids", s.putBodies)
	}
}

func TestAddAssigneesRejectsEmptyLoginList(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.AddAssignees(context.Background(), "o", "r", 5, nil); err == nil {
		t.Error("AddAssignees with no logins: want error, got nil")
	}
}

func TestAddAssigneesRejectsNonPositiveNumber(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.AddAssignees(context.Background(), "o", "r", 0, []string{"bob"}); err == nil {
		t.Error("AddAssignees with number 0: want error, got nil")
	}
}

func TestAddAssigneesPropagatesMRLookupFailure(t *testing.T) {
	s := &assignServer{mrStatus: http.StatusInternalServerError}
	c := s.client(t)

	if _, err := c.AddAssignees(context.Background(), "o", "r", 5, []string{"bob"}); !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
}

func TestRequestReviewersUnionsWithCurrent(t *testing.T) {
	s := &assignServer{
		reviewers: []participant{{ID: 1, Username: "alice"}},
		users:     map[string]int{"bob": 2},
	}
	c := s.client(t)

	got, err := c.RequestReviewers(context.Background(), "o", "r", 5, []string{"bob"}, nil)
	if err != nil {
		t.Fatalf("RequestReviewers: %v", err)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(sortedStrings(got.Users), want) {
		t.Errorf("got.Users = %v, want %v", got.Users, want)
	}
}

func TestRemoveReviewRequestsDropsMatchingID(t *testing.T) {
	s := &assignServer{
		reviewers: []participant{{ID: 1, Username: "alice"}, {ID: 2, Username: "bob"}},
		users:     map[string]int{"bob": 2},
	}
	c := s.client(t)

	got, err := c.RemoveReviewRequests(context.Background(), "o", "r", 5, []string{"bob"}, nil)
	if err != nil {
		t.Fatalf("RemoveReviewRequests: %v", err)
	}
	want := []string{"alice"}
	if !reflect.DeepEqual(got.Users, want) {
		t.Errorf("got.Users = %v, want %v", got.Users, want)
	}
}

func TestRequestReviewersRejectsTeams(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.RequestReviewers(context.Background(), "o", "r", 5, nil, []string{"platform-team"}); !errors.Is(err, ErrNoTeamReviewers) {
		t.Errorf("err = %v, want ErrNoTeamReviewers", err)
	}
}

func TestRemoveReviewRequestsRejectsTeams(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.RemoveReviewRequests(context.Background(), "o", "r", 5, nil, []string{"platform-team"}); !errors.Is(err, ErrNoTeamReviewers) {
		t.Errorf("err = %v, want ErrNoTeamReviewers", err)
	}
}

func TestRequestReviewersRejectsEmptyUsersAndTeams(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.RequestReviewers(context.Background(), "o", "r", 5, nil, nil); err == nil {
		t.Error("RequestReviewers with no users or teams: want error, got nil")
	}
}

func TestRequestReviewersRejectsNonPositiveNumber(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.RequestReviewers(context.Background(), "o", "r", 0, []string{"bob"}, nil); err == nil {
		t.Error("RequestReviewers with number 0: want error, got nil")
	}
}

func TestRequestReviewersPropagatesUserLookupFailure(t *testing.T) {
	s := &assignServer{userStatus: http.StatusInternalServerError}
	c := s.client(t)

	if _, err := c.RequestReviewers(context.Background(), "o", "r", 5, []string{"bob"}, nil); !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
}
