package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/assignment"
	"github.com/opentalon/talooner/internal/check"
	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/command"
	"github.com/opentalon/talooner/internal/comment"
	"github.com/opentalon/talooner/internal/event"
	"github.com/opentalon/talooner/internal/github"
	"github.com/opentalon/talooner/internal/review"
)

const (
	testToken = "ghs_test_token"
	testKey   = "otk_test_key"
	ruleset   = "rule \"needs description\" { }\n"
)

// fakeCluster is a PluginService answering from a per-action script and
// recording every call, so a test can assert what did *not* happen as easily as
// what did.
type fakeCluster struct {
	pluginpb.UnimplementedPluginServiceServer

	mu       sync.Mutex
	calls    []*pluginpb.ToolCallRequest
	answers  map[string]proto.Message
	failures map[string]string
	// planAnswer, when set, is what evaluate_pr returns for mode=plan instead of
	// answers[ActionEvaluatePR] — the execute-mode script, which would carry
	// Actions rather than Plan and trip EvaluatePR's own "a plan mode response
	// may not carry executable actions" check. A test exercising E2 sets this.
	planAnswer proto.Message
}

func (f *fakeCluster) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	if msg, ok := f.failures[req.GetAction()]; ok {
		return &pluginpb.ToolResultResponse{CallId: req.GetId(), Error: msg}, nil
	}
	answer, ok := f.answers[req.GetAction()]
	if req.GetAction() == cluster.ActionEvaluatePR && req.GetArgs()["mode"] == "plan" && f.planAnswer != nil {
		answer, ok = f.planAnswer, true
	}
	if !ok {
		return &pluginpb.ToolResultResponse{CallId: req.GetId(), Error: "talooner: unknown action " + req.GetAction()}, nil
	}
	raw, err := protojson.Marshal(answer)
	if err != nil {
		return nil, err
	}
	return &pluginpb.ToolResultResponse{CallId: req.GetId(), StructuredContent: string(raw)}, nil
}

// actions returns the action names the cluster was asked for, handshake aside.
func (f *fakeCluster) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, c := range f.calls {
		if c.GetAction() != cluster.ActionWhoami {
			names = append(names, c.GetAction())
		}
	}
	return names
}

func (f *fakeCluster) argsOf(t *testing.T, action string) map[string]string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.GetAction() == action {
			return c.GetArgs()
		}
	}
	t.Fatalf("no %s call was made; calls were %v", action, f.calls)
	return nil
}

// dialFake serves f on loopback and returns a dialled, handshaken client.
func dialFake(t *testing.T, f *fakeCluster) *cluster.Client {
	t.Helper()
	if f.answers == nil {
		f.answers = map[string]proto.Message{}
	}
	if _, ok := f.answers[cluster.ActionWhoami]; !ok {
		f.answers[cluster.ActionWhoami] = &taloonerpb.WhoamiResponse{
			Tenant:          "acme",
			ProtocolVersion: taloonerpb.ProtocolVersion,
			Features:        []string{cluster.FeatureLLMReview},
		}
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }() //nolint:errcheck // stopped below
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := cluster.Dial(ctx, "http://"+lis.Addr().String(), testKey)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() }) //nolint:errcheck // test teardown
	return c
}

// fakeGitHub serves the endpoints a run touches and records the paths hit and
// the check runs written.
type fakeGitHub struct {
	mu       sync.Mutex
	paths    []string
	methods  []string
	checks   []writtenCheck
	comments []writtenComment
	// nextCommentID assigns each created comment a distinct id, so a later
	// sticky write in the same run can find and edit it back — the same thing
	// a real listing would return after a real post (github.UpsertComment,
	// "the unhappy paths are the point").
	nextCommentID int64

	permission string // the collaborator permission level to report
	prStatus   int    // non-zero to fail the pull request fetch
	noRuleset  bool
	headSHA    string
	checkFails bool // true to fail the check run write
	failFiles  bool // true to fail the changed-files call, i.e. fact extraction
	// checkRunID is the check run already at the sha; 0 means there is none.
	checkRunID int64
	// existing is what the comments listing returns, i.e. what earlier runs and
	// humans left on the PR.
	existing     []existingComment
	commentFails bool // true to fail every comment write
	// standing is what the reviews listing returns: what earlier runs left, and
	// what humans left alongside them.
	standing    []existingReview
	reviews     []submittedReview
	dismissed   []string // the ids of the reviews this run retracted
	reviewFails bool     // true to fail the review submit
	// assignees and requested are what the PR carries when the run starts.
	assignees      []string
	requestedUsers []string
	requestedTeams []string
	// ignored is the logins GitHub accepts and then leaves out, i.e. the ones
	// with no write access to the repo.
	ignored        []string
	assigneeWrites []peopleWrite
	reviewerWrites []peopleWrite

	config      string // the tenant config.yaml body; "" means no file
	configFails bool   // true to fail the config read with a 500

	modules      string // the tenant modules.yaml body; "" means no file
	modulesFails bool   // true to fail the modules read with a 500
	teams        string // the tenant teams.yaml body; "" means no file
	teamsFails   bool   // true to fail the teams read with a 500
	files        string // the PR's changed-files body; "" means the default

	architecture string // the tenant architecture.yaml body; "" means no file
	// docs is code_unit doc content, keyed by the exact base-branch path
	// (Phase 2's resolveCodeUnits) — a path absent here 404s like any other
	// unrecognised /contents/ read.
	docs map[string]string

	// fork makes the fixture PR's head repo a different one from its base repo,
	// which is what pr.IsFork keys on and what turns on E2's plan comparison.
	fork bool
	// headRuleset is the head branch's own .github/talooner/rules.tln, read only
	// when fork is true; "" defaults to the same body as the base ruleset, i.e. a
	// fork carrying no rule change of its own. noHeadRuleset overrides it to a 404.
	headRuleset   string
	noHeadRuleset bool
}

// peopleWrite is one assignee or review-request write as it reached GitHub.
type peopleWrite struct {
	method string
	Users  []string `json:"assignees"`
	Teams  []string `json:"team_reviewers"`
}

// existingReview is a review already on the PR when the run starts. It feeds
// both the retraction listing (Body, State) and, since C7, the review.* fact
// extractor (Login, CommitID) reading the very same endpoint.
type existingReview struct {
	ID       int64         `json:"id"`
	Body     string        `json:"body"`
	State    string        `json:"state"`
	CommitID string        `json:"commit_id"`
	User     *existingUser `json:"user,omitempty"`
}

type existingUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// submittedReview is one review submit as it reached GitHub.
type submittedReview struct {
	CommitID string `json:"commit_id"`
	Body     string `json:"body"`
	Event    string `json:"event"`
}

// existingComment is a comment already on the PR when the run starts.
type existingComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// writtenComment is one comment write as it reached GitHub.
type writtenComment struct {
	created bool   // POST rather than PATCH
	id      int64  // the comment edited; 0 on a create
	Body    string `json:"body"`
}

// writtenCheck is one check run write as it reached GitHub.
type writtenCheck struct {
	created    bool
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		Title       string `json:"title"`
		Summary     string `json:"summary"`
		Annotations []struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			Message   string `json:"message"`
		} `json:"annotations"`
	} `json:"output"`
}

func (g *fakeGitHub) client(t *testing.T) *github.Client {
	t.Helper()
	if g.permission == "" {
		g.permission = "write"
	}
	if g.headSHA == "" {
		g.headSHA = "abc123"
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.paths = append(g.paths, r.URL.Path)
		g.methods = append(g.methods, r.Method)
		g.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == http.MethodGet:
			if g.checkRunID == 0 {
				_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":%d,"name":"talooner"}]}`, g.checkRunID)

		case strings.Contains(r.URL.Path, "/check-runs"):
			if g.checkFails {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			var got writtenCheck
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode check run: %v", err)
			}
			got.created = r.Method == http.MethodPost
			g.mu.Lock()
			g.checks = append(g.checks, got)
			g.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"id":991}`)

		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet:
			g.mu.Lock()
			raw, err := json.Marshal(g.existing)
			g.mu.Unlock()
			if err != nil {
				t.Errorf("encode existing comments: %v", err)
			}
			_, _ = w.Write(raw)

		case strings.Contains(r.URL.Path, "/comments"):
			if g.commentFails {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			var got writtenComment
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode comment: %v", err)
			}
			got.created = r.Method == http.MethodPost

			g.mu.Lock()
			if got.created {
				// A real listing would show a just-created comment to the next
				// caller; g.existing plays that role here too, not only the
				// fixture for what an earlier run left (TestReviewCommand...).
				g.nextCommentID++
				got.id = 900 + g.nextCommentID
				g.existing = append(g.existing, existingComment{ID: got.id, Body: got.Body})
			} else {
				id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Sscan(id, &got.id) //nolint:errcheck // a bad id fails the assertions below
				for i := range g.existing {
					if g.existing[i].ID == got.id {
						g.existing[i].Body = got.Body
					}
				}
			}
			g.comments = append(g.comments, got)
			g.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"id":%d}`, got.id)

		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodGet:
			raw, err := json.Marshal(g.standing)
			if err != nil {
				t.Errorf("encode standing reviews: %v", err)
			}
			_, _ = w.Write(raw)

		case strings.HasSuffix(r.URL.Path, "/dismissals"):
			parts := strings.Split(r.URL.Path, "/")
			g.mu.Lock()
			g.dismissed = append(g.dismissed, parts[len(parts)-2])
			g.mu.Unlock()
			_, _ = fmt.Fprint(w, `{}`)

		case strings.HasSuffix(r.URL.Path, "/reviews"):
			if g.reviewFails {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			var got submittedReview
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode review: %v", err)
			}
			g.mu.Lock()
			g.reviews = append(g.reviews, got)
			g.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"id":777}`)

		case strings.HasSuffix(r.URL.Path, "/assignees"):
			var got peopleWrite
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode assignees: %v", err)
			}
			got.method = r.Method
			g.mu.Lock()
			if r.Method == http.MethodPost {
				for _, login := range got.Users {
					if !slices.Contains(g.ignored, login) {
						g.assignees = append(g.assignees, login)
					}
				}
			} else {
				g.assignees = slices.DeleteFunc(g.assignees, func(login string) bool {
					return slices.Contains(got.Users, login)
				})
			}
			g.assigneeWrites = append(g.assigneeWrites, got)
			body := assigneesJSON(g.assignees)
			g.mu.Unlock()
			_, _ = fmt.Fprint(w, body)

		case strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
			var got struct {
				Users []string `json:"reviewers"`
				Teams []string `json:"team_reviewers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode review requests: %v", err)
			}
			g.mu.Lock()
			if r.Method == http.MethodPost {
				g.requestedUsers = append(g.requestedUsers, got.Users...)
				g.requestedTeams = append(g.requestedTeams, got.Teams...)
			} else {
				g.requestedUsers = slices.DeleteFunc(g.requestedUsers, func(l string) bool {
					return slices.Contains(got.Users, l)
				})
				g.requestedTeams = slices.DeleteFunc(g.requestedTeams, func(s string) bool {
					return slices.Contains(got.Teams, s)
				})
			}
			g.reviewerWrites = append(g.reviewerWrites, peopleWrite{method: r.Method, Users: got.Users, Teams: got.Teams})
			body := reviewersJSON(g.requestedUsers, g.requestedTeams)
			g.mu.Unlock()
			_, _ = fmt.Fprint(w, body)

		case strings.HasSuffix(r.URL.Path, "/permission"):
			_, _ = fmt.Fprintf(w, `{"permission":%q}`, g.permission)

		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/rules.tln"):
			// A fork test's head-sha read (E2) is distinguished by ref, not by
			// path — the endpoint is the same one the base-ref read above uses,
			// which is the whole point: GitHub serves a PR's head commit through
			// the base repo once it is opened (auth.md).
			if g.fork && r.URL.Query().Get("ref") == g.headSHA {
				if g.noHeadRuleset {
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
					return
				}
				body := g.headRuleset
				if body == "" {
					body = ruleset
				}
				_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
					len(body), base64.StdEncoding.EncodeToString([]byte(body)))
				return
			}
			if g.noRuleset {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(ruleset), base64.StdEncoding.EncodeToString([]byte(ruleset)))

		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/config.yaml"):
			if g.configFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			if g.config == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(g.config), base64.StdEncoding.EncodeToString([]byte(g.config)))

		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/modules.yaml"):
			if g.modulesFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			if g.modules == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(g.modules), base64.StdEncoding.EncodeToString([]byte(g.modules)))

		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/teams.yaml"):
			if g.teamsFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			if g.teams == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(g.teams), base64.StdEncoding.EncodeToString([]byte(g.teams)))

		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/architecture.yaml"):
			if g.architecture == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(g.architecture), base64.StdEncoding.EncodeToString([]byte(g.architecture)))

		// Any other contents read — CODEOWNERS and code_unit docs among them — is
		// an answer, not a file, unless g.docs names it: the run fetches
		// .github/CODEOWNERS / CODEOWNERS / docs/CODEOWNERS for user.owner, and a
		// code unit's doc_ref for llm_review; either 404s like a real miss when
		// not stubbed. This case is last among the /contents/ branches, so the
		// ruleset and config branches above still win for their exact paths.
		case strings.Contains(r.URL.Path, "/contents/"):
			for path, body := range g.docs {
				if strings.HasSuffix(r.URL.Path, "/contents/"+path) {
					_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
						len(body), base64.StdEncoding.EncodeToString([]byte(body)))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
			return

		case strings.HasSuffix(r.URL.Path, "/files"):
			if g.failFiles {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			// The default PR touches one path under internal/auth/ with line
			// counts, so module selection has something to sum; a test that needs
			// a different set points g.files at its own body.
			body := g.files
			if body == "" {
				body = `[{"filename":"internal/auth/token.go","additions":9,"deletions":1}]`
			}
			_, _ = fmt.Fprint(w, body)

		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = fmt.Fprint(w, `{"state":"success","total_count":0,"statuses":[]}`)

		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)

		case strings.HasSuffix(r.URL.Path, "/commits"):
			// No prior commit history for any path: the git-log tier of
			// user.owner resolution finds nothing, same as a repo with no
			// CODEOWNERS — none of these tests assert on user.owner.
			_, _ = fmt.Fprint(w, `[]`)

		default: // the pull request itself
			if g.prStatus != 0 {
				w.WriteHeader(g.prStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			g.mu.Lock()
			assignees, users, teams := loginsJSON(g.assignees), loginsJSON(g.requestedUsers), slugsJSON(g.requestedTeams)
			headRepo := "opentalon/talooner"
			if g.fork {
				headRepo = "attacker/talooner"
			}
			g.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{
				"number": 42,
				"head": {"sha": %q, "ref": "feat/x", "repo": {"full_name": %q}},
				"base": {"sha": "def456", "ref": "master", "repo": {"full_name": "opentalon/talooner"}},
				"user": {"login": "evgeny"},
				"title": "Add a thing", "body": "", "state": "open", "mergeable": true,
				"additions": 10, "deletions": 3, "changed_files": 1, "commits": 2,
				"assignees": %s, "requested_reviewers": %s, "requested_teams": %s
			}`, g.headSHA, headRepo, assignees, users, teams)
		}
	}))
	t.Cleanup(srv.Close)

	// No retries: a test asserting on a 5xx should not also wait out the backoff.
	c, err := github.New(testToken,
		github.WithBaseURL(srv.URL), github.WithHTTPClient(srv.Client()), github.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return c
}

func (g *fakeGitHub) hit(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.paths {
		if strings.Contains(p, path) {
			return true
		}
	}
	return false
}

// check returns the one check run the run wrote, failing the test if it wrote
// none or more than one.
func (g *fakeGitHub) check(t *testing.T) writtenCheck {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.checks) != 1 {
		t.Fatalf("check runs written = %d, want exactly 1: %+v", len(g.checks), g.checks)
	}
	return g.checks[0]
}

// ackBody is the raw text of the `!talooner /review` acknowledgement as it
// reaches GitHub: the review marker plus Acknowledge(), the same shape any
// other TopicReview write has (github.StickyComment.text).
var ackBody = comment.Marker(comment.TopicReview) + "\n" + comment.Acknowledge()

// verdictComments is every comment write the run made, minus the initial
// acknowledgement post — every comment assertion in this file predates the
// ack and is not testing for it. Once report() runs, the ack has already been
// edited into whatever it landed on, so this is never the ack's own final
// state, only the POST that preceded it.
func (g *fakeGitHub) verdictComments() []writtenComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []writtenComment
	for _, c := range g.comments {
		if c.Body == ackBody {
			continue
		}
		out = append(out, c)
	}
	return out
}

// wrote returns the one verdict comment the run wrote, failing the test if it
// wrote none or more than one.
func (g *fakeGitHub) wrote(t *testing.T) writtenComment {
	t.Helper()
	got := g.verdictComments()
	if len(got) != 1 {
		t.Fatalf("comments written = %d, want exactly 1: %+v", len(got), got)
	}
	return got[0]
}

func commentEvent(body string) *event.Event {
	return &event.Event{
		Trigger:     event.TriggerIssueComment,
		Action:      "created",
		Owner:       "opentalon",
		Repo:        "talooner",
		PR:          42,
		Actor:       "evgeny",
		CommentBody: body,
		CommentID:   7,
	}
}

func evaluated(actions ...*taloonerpb.Action) map[string]proto.Message {
	return map[string]proto.Message{
		cluster.ActionSetSubscription: &taloonerpb.SetSubscriptionResponse{Subscribed: true},
		cluster.ActionIsSubscribed:    &taloonerpb.IsSubscribedResponse{Subscribed: true},
		cluster.ActionEvaluatePR: &taloonerpb.EvaluatePrResponse{
			Actions: actions,
			Explain: &taloonerpb.Explain{Summary: "1 rule fired"},
		},
	}
}

func TestReviewCommandRunsTheWholeSpine(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "describe your change",
	})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{cluster.ActionSetSubscription, cluster.ActionEvaluatePR}
	if got := f.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cluster calls = %v, want %v", got, want)
	}

	args := f.argsOf(t, cluster.ActionEvaluatePR)
	if args["repo"] != "opentalon/talooner" || args["pr"] != "42" || args["head_sha"] != "abc123" {
		t.Errorf("scope args = %v", args)
	}
	if args["ruleset"] != ruleset {
		t.Errorf("ruleset arg = %q, want the base-branch file", args["ruleset"])
	}
	if args["mode"] != string(cluster.ModeExecute) {
		t.Errorf("mode = %q, want execute", args["mode"])
	}

	var set map[string]any
	if err := json.Unmarshal([]byte(args["facts"]), &set); err != nil {
		t.Fatalf("facts arg: %v", err)
	}
	// The extractor's whole set travels, negative cases included: an unset fact
	// reads as false cluster-side, so a missing one is a wrong answer.
	for _, name := range []string{"pr.number", "pr.head_sha", "pr.changed_files", "pr.has_description", "pr.draft"} {
		if _, ok := set[name]; !ok {
			t.Errorf("facts is missing %s", name)
		}
	}
	if set["pr.has_description"] != false {
		t.Errorf("pr.has_description = %v, want false for an empty body", set["pr.has_description"])
	}
}

// A manual `!talooner /review` can take long enough that, without an
// immediate reply, the commander sees nothing happen until the verdict lands
// — or goes looking in the Actions tab, which most never open. So the very
// first thing after subscribing is a plain "on it" comment, ahead of the
// ruleset load and the cluster round trip that follow.
func TestReviewCommandAcknowledgesBeforeEvaluating(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gh.comments) == 0 || gh.comments[0].Body != ackBody {
		t.Fatalf("first comment written = %+v, want the acknowledgement first", gh.comments)
	}
	if !gh.comments[0].created {
		t.Error("the acknowledgement must be a new comment, not an edit")
	}
}

// The ack is posted on the review topic itself, so once report() writes the
// real verdict it edits the very same comment rather than leaving the ack
// standing next to it — no stale "Evaluating…" left behind, and no second
// comment either.
func TestReviewCommandEditsAcknowledgementIntoVerdict(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "describe your change",
	})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gh.comments) != 2 {
		t.Fatalf("comments written = %+v, want exactly 2: the ack, then the edit that replaces it", gh.comments)
	}
	ack, edit := gh.comments[0], gh.comments[1]
	if !ack.created || ack.Body != ackBody {
		t.Fatalf("first write = %+v, want the ack, created", ack)
	}
	if edit.created || edit.id != ack.id {
		t.Fatalf("second write = %+v, want an edit of the ack's own comment (id %d)", edit, ack.id)
	}
	if strings.Contains(edit.Body, "Evaluating") {
		t.Error("the ack text is still there; it should have been replaced by the verdict")
	}
	if !strings.Contains(edit.Body, "describe your change") {
		t.Errorf("edited comment does not carry the finding:\n%s", edit.Body)
	}
}

// A failure to post the acknowledgement is not worth failing an otherwise
// working run over — the verdict itself is what matters.
func TestReviewCommandStillRunsIfAcknowledgeFails(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{commentFails: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v, want the acknowledgement failure to be swallowed", err)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionSuccess {
		t.Errorf("conclusion = %q, want success — an unwritable ack must not fail the run", got)
	}
}

// Without architecture.yaml, code_unit doc loading is not attempted at all —
// even though the default fixture's changed file (internal/auth/token.go)
// matches the built-in service layer and resolves a doc_ref by convention.
// This is the opt-in gate (expert-review-system.md, Phase 2): the built-in
// layer table matches almost every Go PR, so resolving docs and warning on
// every one with no docs/services/*.md would spam every onboarded repo that
// never asked for llm_review. architecture.yaml, even a one-line rule for
// something else entirely, is the signal that a repo wants this.
func TestCodeUnitsOmittedWithoutArchitectureYaml(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{} // no architecture.yaml, and no docs/services/auth.md stubbed either

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := f.argsOf(t, cluster.ActionEvaluatePR)
	if _, ok := args["code_units"]; ok {
		t.Errorf("code_units arg present = %q, want absent with no architecture.yaml", args["code_units"])
	}
	// Nothing to warn about and nothing to report — the acknowledgement is
	// edited to say so rather than left standing.
	got := gh.wrote(t)
	if !strings.Contains(got.Body, "Nothing to report") {
		t.Errorf("comment = %q, want it resolved to nothing to report", got.Body)
	}
}

// With architecture.yaml present (the opt-in) and the built-in convention's doc
// actually on the base branch, evaluate_pr's code_units arg carries the unit
// with its doc content inline — the shape talooner-plugin's units.go decodes
// (name, important, doc_url, doc_content, diff).
func TestCodeUnitsSentWithDocContentWhenArchitectureYamlPresent(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{
		// This rule does not touch internal/auth at all: the built-in layer
		// table still resolves it by convention. architecture.yaml's mere
		// presence is the opt-in signal, not what it says.
		architecture: "- path: unrelated/\n  kind: service\n",
		docs:         map[string]string{"docs/services/auth.md": "auth must hash passwords, never log them"},
	}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := f.argsOf(t, cluster.ActionEvaluatePR)
	raw, ok := args["code_units"]
	if !ok {
		t.Fatalf("no code_units arg sent, args = %v", args)
	}
	var units []map[string]any
	if err := json.Unmarshal([]byte(raw), &units); err != nil {
		t.Fatalf("code_units arg is not JSON: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("code_units = %v, want one unit", units)
	}
	want := map[string]any{
		"name": "internal/auth", "important": true, "doc_url": "docs/services/auth.md",
		"doc_content": "auth must hash passwords, never log them",
	}
	for k, v := range want {
		if units[0][k] != v {
			t.Errorf("code_units[0][%q] = %v, want %v", k, units[0][k], v)
		}
	}
	// The doc was found, so nothing warns; the acknowledgement is edited to
	// say there is nothing to report rather than left standing.
	edited := gh.wrote(t)
	if !strings.Contains(edited.Body, "Nothing to report") {
		t.Errorf("comment = %q, want it resolved to nothing to report", edited.Body)
	}
}

// A doc_ref the base branch does not have — architecture.yaml opted in, but
// docs/services/auth.md was never written — drops the unit from code_units
// (nothing to review it against) and warns in the sticky comment rather than
// failing the run: llm_review is additive, never a reason to withhold the rest
// of the verdict.
func TestMissingCodeUnitDocWarnsButDoesNotFailTheRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{architecture: "- path: unrelated/\n  kind: service\n"} // opted in, no docs/services/auth.md stubbed

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := f.argsOf(t, cluster.ActionEvaluatePR)
	if raw, ok := args["code_units"]; ok {
		t.Errorf("code_units arg = %q, want absent — the only unit's doc is missing", raw)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionSuccess {
		t.Errorf("conclusion = %q, want %q — a missing doc must not fail the run", got, github.ConclusionSuccess)
	}
	got := gh.wrote(t)
	if !strings.Contains(got.Body, "code_unit_doc_unavailable") || !strings.Contains(got.Body, "docs/services/auth.md") {
		t.Errorf("comment does not warn about the missing doc:\n%s", got.Body)
	}
}

// A comment that is not addressed to Talooner must not reach GitHub at all. The
// permission call is the one to watch: it is what tells an unauthorised account
// the bot is installed.
// A verdict this build cannot decode — an action with no verb, or one from a
// newer cluster — fails the run. Logging it and carrying on would report success
// for a decision that was never carried out.
func TestBlockWritesAFailingCheckRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr.merge",
	})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := gh.check(t)
	if got.Conclusion != github.ConclusionFailure {
		t.Errorf("conclusion = %q, want failure", got.Conclusion)
	}
	if got.Name != check.Name || got.HeadSHA != "abc123" {
		t.Errorf("check run identity = %s@%s", got.Name, got.HeadSHA)
	}
	if !got.created {
		t.Error("the first run at a sha creates the check run")
	}
}

// The second run at a sha updates the first one's check run. Thirty pushes give
// one talooner check, not thirty.
func TestASecondRunUpdatesTheSameCheckRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr",
	})}
	gh := &fakeGitHub{checkRunID: 4242}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := gh.check(t)
	if got.created {
		t.Fatal("a check run already exists at this sha; it must be updated, not duplicated")
	}
	if got.Conclusion != github.ConclusionSuccess {
		t.Errorf("conclusion = %q, want success", got.Conclusion)
	}
}

// No rule firing is a verdict too. Writing nothing would leave the previous
// run's verdict standing at a sha it no longer describes.
func TestNoActionsStillWritesACheckRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := gh.check(t); got.Conclusion != github.ConclusionSuccess {
		t.Errorf("conclusion = %q, want success", got.Conclusion)
	}
}

// A repo that has not onboarded gets no check at all: a neutral talooner check
// on a repo that never asked for one is noise. It gets one comment instead
// (E1, #20), saying so.
func TestMissingRulesetWritesNoCheckRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{noRuleset: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.checks) != 0 {
		t.Errorf("check runs written = %+v, want none", gh.checks)
	}
	got := gh.wrote(t)
	if !strings.HasPrefix(got.Body, comment.Marker(comment.TopicReview)) {
		t.Errorf("comment does not start with the review marker:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, RulesetPath) {
		t.Errorf("comment does not name %s:\n%s", RulesetPath, got.Body)
	}
}

// C3 reads config.yaml from the base branch. A valid file is accepted; a
// malformed one fails the run but still writes the neutral check, the same
// fail-open shape as a broken ruleset (D2).
func TestConfigRead(t *testing.T) {
	t.Run("valid config is accepted", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{config: "checks:\n  tests: [\"test\"]\n  lint: [\"lint\"]\n"}

		if err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("malformed config fails the run, neutral, and is explained in a comment", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{config: "checks: [\n"}

		err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		})
		if err == nil {
			t.Fatal("Run = nil, want the config parse failure")
		}
		if got := gh.check(t); got.Conclusion != github.ConclusionNeutral {
			t.Errorf("conclusion = %q, want neutral: a tenant config error is not a policy outcome", got.Conclusion)
		}
		got := gh.wrote(t)
		if !strings.Contains(got.Body, ConfigPath) {
			t.Errorf("comment does not name %s:\n%s", ConfigPath, got.Body)
		}
	})

	t.Run("a config field shaped like a credential fails the run", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{config: "checks:\n  tests: [\"test\"]\napi_key: xyz\n"}

		err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		})
		if err == nil {
			t.Fatal("Run = nil, want the credential-field rejection")
		}
	})
}

// C6 reads modules.yaml and teams.yaml from the base branch and feeds
// module.* and the require resolver. The PR here touches internal/auth/token.go
// (the default /files response), which modules.yaml owns.
func TestModuleFacts(t *testing.T) {
	const modules = "- path: internal/auth/\n  documentation_url: https://docs/auth\n  owner: \"@alice\"\n"

	t.Run("configured module touched yields module.* facts", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{modules: modules}

		if err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var set map[string]any
		if err := json.Unmarshal([]byte(f.argsOf(t, cluster.ActionEvaluatePR)["facts"]), &set); err != nil {
			t.Fatalf("facts arg: %v", err)
		}
		if set["module.touched_count"] != float64(1) {
			t.Errorf("module.touched_count = %v, want 1", set["module.touched_count"])
		}
		if set["module.documentation_url"] != "https://docs/auth" {
			t.Errorf("module.documentation_url = %v, want the auth docs", set["module.documentation_url"])
		}
		if set["module.owner"] != "@alice" {
			t.Errorf("module.owner = %v, want @alice", set["module.owner"])
		}
		urls, ok := set["module.documentation_urls"].([]any)
		if !ok || len(urls) != 1 || urls[0] != "https://docs/auth" {
			t.Errorf("module.documentation_urls = %v, want [https://docs/auth]", set["module.documentation_urls"])
		}
	})

	t.Run("no modules.yaml yields touched_count 0, rest unset", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{}

		if err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var set map[string]any
		if err := json.Unmarshal([]byte(f.argsOf(t, cluster.ActionEvaluatePR)["facts"]), &set); err != nil {
			t.Fatalf("facts arg: %v", err)
		}
		if set["module.touched_count"] != float64(0) {
			t.Errorf("module.touched_count = %v, want 0", set["module.touched_count"])
		}
		for _, name := range []string{"module.documentation_url", "module.documentation_urls", "module.owner"} {
			if _, ok := set[name]; ok {
				t.Errorf("%s = %v, want unset", name, set[name])
			}
		}
	})

	t.Run("malformed modules.yaml fails the run, neutral", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{modules: "path: [\n"}

		err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		})
		if err == nil {
			t.Fatal("Run = nil, want the modules parse failure")
		}
		if got := gh.check(t); got.Conclusion != github.ConclusionNeutral {
			t.Errorf("conclusion = %q, want neutral: a tenant module error is not a policy outcome", got.Conclusion)
		}
		got := gh.wrote(t)
		if !strings.Contains(got.Body, ModulePath) {
			t.Errorf("comment does not name %s:\n%s", ModulePath, got.Body)
		}
	})

	t.Run("a modules.yaml path escaping the repo fails the run", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{modules: "- path: ../../etc\n"}

		err := Run(t.Context(), Runner{
			Event:   commentEvent("!talooner /review"),
			GitHub:  gh.client(t),
			Cluster: dialFake(t, f),
		})
		if err == nil {
			t.Fatal("Run = nil, want the path-escape rejection")
		}
	})
}

// review.* facts are re-derived from the PR's current review list every run,
// through the same GET /reviews endpoint the retraction sync uses.
func TestReviewFacts(t *testing.T) {
	t.Run("non-bot approval at head sha", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{standing: []existingReview{
			{ID: 1, State: github.StateApproved, CommitID: "abc123", User: &existingUser{Login: "alice", Type: "User"}},
		}}

		if err := Run(t.Context(), Runner{
			Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var set map[string]any
		if err := json.Unmarshal([]byte(f.argsOf(t, cluster.ActionEvaluatePR)["facts"]), &set); err != nil {
			t.Fatalf("facts arg: %v", err)
		}
		if set["review.human.approved"] != true {
			t.Errorf("review.human.approved = %v, want true", set["review.human.approved"])
		}
	})

	t.Run("a bot's approval does not count as human", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{standing: []existingReview{
			{ID: 1, State: github.StateApproved, CommitID: "abc123", User: &existingUser{Login: "dependabot", Type: "Bot"}},
		}}

		if err := Run(t.Context(), Runner{
			Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var set map[string]any
		if err := json.Unmarshal([]byte(f.argsOf(t, cluster.ActionEvaluatePR)["facts"]), &set); err != nil {
			t.Fatalf("facts arg: %v", err)
		}
		if set["review.human.approved"] != false {
			t.Errorf("review.human.approved = %v, want false", set["review.human.approved"])
		}
	})

	t.Run("no reviews at all yields false, not unset", func(t *testing.T) {
		f := &fakeCluster{answers: evaluated()}
		gh := &fakeGitHub{}

		if err := Run(t.Context(), Runner{
			Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f),
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var set map[string]any
		if err := json.Unmarshal([]byte(f.argsOf(t, cluster.ActionEvaluatePR)["facts"]), &set); err != nil {
			t.Fatalf("facts arg: %v", err)
		}
		if _, ok := set["review.human.approved"]; !ok {
			t.Error("review.human.approved missing, want an explicit false")
		}
		if set["review.changes_requested"] != false {
			t.Errorf("review.changes_requested = %v, want false", set["review.changes_requested"])
		}
	})
}

// teams.yaml sits in front of the path-derived require target. A ruleset that
// asks for review.senior_oncall resolves to the team the repo mapped it to, not
// a "senior_oncall" slug, and the mapped value may name a team in another org.
func TestTeamsYamlResolvesRequire(t *testing.T) {
	const teams = "senior_oncall: \"@org/security\"\npayments: \"@org/payments\"\n"

	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_REQUIRE, Target: "review.senior_oncall"},
	)}
	gh := &fakeGitHub{teams: teams}

	if err := Run(t.Context(), Runner{
		Event:   commentEvent("!talooner /review"),
		GitHub:  gh.client(t),
		Cluster: dialFake(t, f),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(gh.requestedTeams, "org/security") {
		t.Errorf("requested teams = %v, want org/security from teams.yaml", gh.requestedTeams)
	}
}

// The hardest requirement of D2: Talooner's own faults are neutral. A repo that
// marked the check required must not be blocked because the bot broke.
func TestABrokenRunWritesNeutralNotFailure(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_UNSPECIFIED})}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if !errors.Is(err, action.ErrUnknownVerb) {
		t.Fatalf("Run returned %v, want action.ErrUnknownVerb: the job still goes red", err)
	}
	got := gh.check(t)
	if got.Conclusion != github.ConclusionNeutral {
		t.Fatalf("conclusion = %q, want neutral: a bot fault is not a policy outcome", got.Conclusion)
	}
	if !strings.Contains(got.Output.Summary, "unknown verb") {
		t.Errorf("summary should name what broke:\n%s", got.Output.Summary)
	}
}

// The same, one step earlier: a run that dies during extraction leaves a neutral
// check where the last run's success was, not the success itself.
func TestExtractionFailureLeavesNoStaleVerdict(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{failFiles: true, checkRunID: 4242}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the extraction failure")
	}
	got := gh.check(t)
	if got.created {
		t.Error("the existing check run must be updated, not duplicated")
	}
	if got.Conclusion != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral", got.Conclusion)
	}
}

// A ruleset the plugin refuses is annotated at the line the compiler names, and
// still neutral: a syntax error in the ruleset is the maintainer's to fix, not a
// reason to block their merge.
func TestBrokenRulesetIsAnnotatedAndNeutral(t *testing.T) {
	f := &fakeCluster{
		answers: map[string]proto.Message{
			cluster.ActionSetSubscription: &taloonerpb.SetSubscriptionResponse{Subscribed: true},
			cluster.ActionValidateRuleset: &taloonerpb.ValidateRulesetResponse{
				Valid: false,
				Diagnostics: []*taloonerpb.Diagnostic{
					{Severity: taloonerpb.Severity_SEVERITY_ERROR, Message: `unexpected token "do"`, Line: 3, Column: 9},
					{Severity: taloonerpb.Severity_SEVERITY_WARNING, Message: "rule never fires", Line: 11},
				},
			},
		},
		failures: map[string]string{cluster.ActionEvaluatePR: "talooner: evaluate ruleset: compile failed"},
	}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the plugin's refusal")
	}
	got := gh.check(t)
	if got.Conclusion != github.ConclusionNeutral {
		t.Fatalf("conclusion = %q, want neutral", got.Conclusion)
	}
	if len(got.Output.Annotations) != 1 {
		t.Fatalf("annotations = %+v, want only the error one", got.Output.Annotations)
	}
	a := got.Output.Annotations[0]
	if a.Path != RulesetPath || a.StartLine != 3 {
		t.Errorf("annotation = %+v, want %s line 3", a, RulesetPath)
	}
	if !strings.Contains(a.Message, "unexpected token") || !strings.Contains(a.Message, "column 9") {
		t.Errorf("annotation message = %q", a.Message)
	}
}

// Diagnostics are a nicety; the neutral check run is not. A cluster that cannot
// answer validate_ruleset still owes the PR a check.
func TestBrokenRulesetWithNoDiagnosticsStillWritesTheCheck(t *testing.T) {
	f := &fakeCluster{
		answers:  map[string]proto.Message{cluster.ActionSetSubscription: &taloonerpb.SetSubscriptionResponse{Subscribed: true}},
		failures: map[string]string{cluster.ActionEvaluatePR: "talooner: evaluate ruleset: compile failed"},
	}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the plugin's refusal")
	}
	got := gh.check(t)
	if got.Conclusion != github.ConclusionNeutral || len(got.Output.Annotations) != 0 {
		t.Errorf("check run = %+v, want a neutral one with no annotations", got)
	}
}

// A check run that could not be written is not worth hiding the real failure
// behind, but a decision that could not be published must still fail the run.
func TestAnUnwritableCheckRunFailsTheRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{checkFails: true}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if err == nil {
		t.Fatal("Run = nil, want the failed check run write")
	}
	if !strings.Contains(err.Error(), "check run") {
		t.Errorf("err = %v, want it to name the check run", err)
	}
}

func TestUndecodableActionFailsTheRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr.merge"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_UNSPECIFIED},
	)}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if !errors.Is(err, action.ErrUnknownVerb) {
		t.Fatalf("Run returned %v, want action.ErrUnknownVerb", err)
	}
}

func TestCommentWithNoCommandTouchesNothing(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("looks good to me"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none", got)
	}
	if len(gh.paths) != 0 {
		t.Errorf("github calls = %v, want none", gh.paths)
	}
}

func TestCommandFromAnAccountWithoutWriteAccessIsIgnored(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{permission: "read"}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run = %v, want nil: an unauthorised command is exit 0 and silence", err)
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none: nothing was subscribed or evaluated", got)
	}
	if gh.hit("/pulls/42") {
		t.Error("fetched the pull request for an unauthorised commander")
	}
}

// A permission API that fails is not a "no". Reading it as one would silently
// drop a maintainer's command.
func TestAuthorizeFailureFailsTheRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	gh, err := github.New(testToken, github.WithBaseURL(srv.URL), github.WithHTTPClient(srv.Client()), github.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh, Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want an error when the permission check breaks")
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none", got)
	}
}

func TestUnknownCommandFromAnAuthorizedUserEvaluatesNothing(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /shipit"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !gh.hit("/permission") {
		t.Error("the commander was never authorized, so a reply would leak the bot to anyone")
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none", got)
	}
}

func TestStopUnsubscribesAndStops(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionSetSubscription: &taloonerpb.SetSubscriptionResponse{Subscribed: false},
	}}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /stop"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); strings.Join(got, ",") != cluster.ActionSetSubscription {
		t.Fatalf("cluster calls = %v, want only set_subscription", got)
	}
	if args := f.argsOf(t, cluster.ActionSetSubscription); args["state"] != "false" {
		t.Errorf("state = %q, want false", args["state"])
	}
	if gh.hit("/pulls/42/files") {
		t.Error("extracted facts for a PR that was being unsubscribed")
	}
}

func TestWhyPostsTheExplanationAndDoesNotEvaluate(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionExplainPR: &taloonerpb.ExplainPrResponse{
			Explain: &taloonerpb.Explain{
				Summary: "blocked: missing description",
				Firings: []*taloonerpb.RuleFiring{{Rule: "needs description", Priority: "HIGH"}},
			},
		},
	}}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /why"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); strings.Join(got, ",") != cluster.ActionExplainPR {
		t.Fatalf("cluster calls = %v, want only explain_pr", got)
	}
	if args := f.argsOf(t, cluster.ActionExplainPR); args["head_sha"] != "abc123" {
		t.Errorf("head_sha = %q, want the PR's current head sha", args["head_sha"])
	}
	c := gh.wrote(t)
	if !c.created {
		t.Error("the why reply must be a new comment, not an edit")
	}
	for _, want := range []string{"blocked: missing description", "needs description"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("comment is missing %q:\n%s", want, c.Body)
		}
	}
}

// A second /why on the same PR is a second question, not an edit to the
// first answer.
func TestWhyTwiceWritesTwoComments(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionExplainPR: &taloonerpb.ExplainPrResponse{Explain: &taloonerpb.Explain{Summary: "ok"}},
	}}
	gh := &fakeGitHub{}
	c := dialFake(t, f)

	for range 2 {
		if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /why"), GitHub: gh.client(t), Cluster: c}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if len(gh.comments) != 2 {
		t.Fatalf("comments written = %d, want 2", len(gh.comments))
	}
}

// A sha nothing was ever evaluated at is a distinct, clear answer, not a
// failed run.
func TestWhyWithNoDecisionRepliesInsteadOfFailing(t *testing.T) {
	f := &fakeCluster{failures: map[string]string{
		cluster.ActionExplainPR: "talooner: no decision recorded for opentalon/talooner#42 at abc123; it was not evaluated at that sha",
	}}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /why"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run = %v, want nil: this is a clear answer, not a run failure", err)
	}
	c := gh.wrote(t)
	if !strings.Contains(c.Body, "no decision recorded") {
		t.Errorf("comment does not carry the plugin's reason:\n%s", c.Body)
	}
}

// A transport failure is not a clear answer and must fail the run like any
// other broken cluster call.
func TestWhyTransportFailureFailsTheRun(t *testing.T) {
	gh := &fakeGitHub{}
	f := &fakeCluster{} // no explain_pr answer scripted: Execute never reaches the plugin's own refusal path
	c := dialFake(t, f)
	c.Close() //nolint:errcheck // deliberately broken to force a transport error

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /why"), GitHub: gh.client(t), Cluster: c})
	if err == nil {
		t.Fatal("Run = nil, want an error when the cluster call fails outright")
	}
	if len(gh.comments) != 0 {
		t.Error("a transport failure must not be answered as if it were a clear refusal")
	}
}

// H2 (#70): the manual `/plan` evaluates the head-branch ruleset in plan mode
// and posts what it would decide right now, with no writes. Different from
// E2's automatic fork-PR diff (TestForkPRPostsTheDecisionDiffAgainstTheBaseRuleset
// et al. above): this has no base decision to compare against, just the plan
// itself, and it never fires automatically.

func TestPlanPostsWhatTheHeadRulesetWouldDecide(t *testing.T) {
	f := &fakeCluster{
		planAnswer: &taloonerpb.EvaluatePrResponse{
			Plan: []*taloonerpb.Action{
				{Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "describe your change"},
				{Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr"},
			},
		},
	}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /plan"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if args := f.argsOf(t, cluster.ActionEvaluatePR); args["mode"] != "plan" {
		t.Errorf("mode = %q, want plan", args["mode"])
	}
	c := gh.wrote(t)
	if !c.created {
		t.Error("the plan reply must be a new comment, not an edit")
	}
	for _, want := range []string{"describe your change", "block pr"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("comment is missing %q:\n%s", want, c.Body)
		}
	}
}

// /plan must never subscribe the PR or touch subscription state at all — only
// /review and /stop do.
func TestPlanDoesNotSubscribe(t *testing.T) {
	f := &fakeCluster{planAnswer: &taloonerpb.EvaluatePrResponse{}}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /plan"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range f.actions() {
		if name == cluster.ActionSetSubscription || name == cluster.ActionIsSubscribed {
			t.Errorf("plan touched subscription state via %s", name)
		}
	}
}

// A ruleset with nothing in it is a legitimate plan outcome, not an error.
func TestPlanWithNoFiringsSaysSo(t *testing.T) {
	f := &fakeCluster{planAnswer: &taloonerpb.EvaluatePrResponse{}}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /plan"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c := gh.wrote(t)
	if !strings.Contains(c.Body, "No rules fired") {
		t.Errorf("comment does not say no rules fired:\n%s", c.Body)
	}
}

// No ruleset on the head branch is an answer, not an error: there is nothing
// to plan.
func TestPlanWithNoHeadRulesetRepliesInsteadOfEvaluating(t *testing.T) {
	f := &fakeCluster{}
	gh := &fakeGitHub{noRuleset: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /plan"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none: no ruleset means nothing to evaluate", got)
	}
	c := gh.wrote(t)
	if !strings.Contains(c.Body, RulesetPath) {
		t.Errorf("comment does not name %s:\n%s", RulesetPath, c.Body)
	}
}

// A head ruleset that will not compile is a clear answer, not a run failure —
// the same call why makes for a sha with no recorded decision.
func TestPlanWithBrokenRulesetRepliesInsteadOfFailing(t *testing.T) {
	f := &fakeCluster{failures: map[string]string{
		cluster.ActionEvaluatePR: "talooner: ruleset does not compile: line 3: unexpected token",
	}}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /plan"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run = %v, want nil: this is a clear answer, not a run failure", err)
	}
	c := gh.wrote(t)
	if !strings.Contains(c.Body, "does not compile") {
		t.Errorf("comment does not carry the plugin's reason:\n%s", c.Body)
	}
}

// A transport failure is not a clear answer and must fail the run like any
// other broken cluster call.
func TestPlanTransportFailureFailsTheRun(t *testing.T) {
	gh := &fakeGitHub{}
	f := &fakeCluster{} // no evaluate_pr answer scripted: Execute never reaches the plugin's own refusal path
	c := dialFake(t, f)
	c.Close() //nolint:errcheck // deliberately broken to force a transport error

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /plan"), GitHub: gh.client(t), Cluster: c})
	if err == nil {
		t.Fatal("Run = nil, want an error when the cluster call fails outright")
	}
	if len(gh.comments) != 0 {
		t.Error("a transport failure must not be answered as if it were a clear refusal")
	}
}

// Every trigger except a comment runs only to serve a PR somebody already
// invoked Talooner on. An unsubscribed one is a skipped job, not a red X.
func TestUnsubscribedPushIsASkipNotAFailure(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionIsSubscribed: &taloonerpb.IsSubscribedResponse{Subscribed: false},
	}}
	gh := &fakeGitHub{}
	ev := &event.Event{
		Trigger: event.TriggerPullRequest, Action: "synchronize",
		Owner: "opentalon", Repo: "talooner", PR: 42, HeadSHA: "abc123", Actor: "evgeny",
	}

	if err := Run(t.Context(), Runner{Event: ev, GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run = %v, want nil for an unsubscribed PR", err)
	}
	if got := f.actions(); strings.Join(got, ",") != cluster.ActionIsSubscribed {
		t.Fatalf("cluster calls = %v, want only is_subscribed", got)
	}
	if len(gh.paths) != 0 {
		t.Errorf("github calls = %v, want none: the cheap path costs one plugin call", gh.paths)
	}
}

func TestSubscribedPushEvaluates(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}
	ev := &event.Event{
		Trigger: event.TriggerCheckSuite, Action: "completed",
		Owner: "opentalon", Repo: "talooner", PR: 42, HeadSHA: "abc123",
	}

	if err := Run(t.Context(), Runner{Event: ev, GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{cluster.ActionIsSubscribed, cluster.ActionEvaluatePR}
	if got := f.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cluster calls = %v, want %v", got, want)
	}
	// An empty action list is still a decision. It must not read as a run that
	// did nothing — D2 turns it into a check run.
	if resp := f.argsOf(t, cluster.ActionEvaluatePR); resp["head_sha"] != "abc123" {
		t.Errorf("head_sha = %q", resp["head_sha"])
	}
}

func TestClosedPullRequestUnsubscribes(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionSetSubscription: &taloonerpb.SetSubscriptionResponse{Subscribed: false},
	}}
	gh := &fakeGitHub{}
	ev := &event.Event{
		Trigger: event.TriggerPullRequest, Action: "closed",
		Owner: "opentalon", Repo: "talooner", PR: 42, HeadSHA: "abc123",
	}

	if err := Run(t.Context(), Runner{Event: ev, GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); strings.Join(got, ",") != cluster.ActionSetSubscription {
		t.Fatalf("cluster calls = %v, want only set_subscription", got)
	}
	if args := f.argsOf(t, cluster.ActionSetSubscription); args["state"] != "false" {
		t.Errorf("state = %q, want false: subscription ends when the PR closes", args["state"])
	}
}

// A repo that has not onboarded has no ruleset. Nothing to evaluate is not a
// failure, and it must not spend a plugin call either.
func TestMissingRulesetIsASkip(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{noRuleset: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run = %v, want nil for a repo with no ruleset", err)
	}
	for _, a := range f.actions() {
		if a == cluster.ActionEvaluatePR {
			t.Fatal("evaluated a PR with no ruleset")
		}
	}
	if gh.hit("/pulls/42/files") {
		t.Error("extracted facts before finding out there is no ruleset")
	}
}

// A run whose fact extraction dies must fail rather than evaluate: the plugin
// re-derives from what arrives, so a short fact set is a wrong verdict, not a
// missing one.
func TestFactExtractionFailureNeverEvaluates(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{prStatus: http.StatusInternalServerError}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if err == nil {
		t.Fatal("Run = nil, want an error when the PR cannot be fetched")
	}
	for _, a := range f.actions() {
		if a == cluster.ActionEvaluatePR {
			t.Fatal("evaluated with a fact set that could not be built")
		}
	}
}

func TestPluginRefusalFailsTheRun(t *testing.T) {
	f := &fakeCluster{
		answers:  evaluated(),
		failures: map[string]string{cluster.ActionEvaluatePR: "talooner: ruleset does not compile: line 3"},
	}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if err == nil {
		t.Fatal("Run = nil, want the plugin's refusal to fail the run")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v, want the plugin's own message", err)
	}
}

// The PR moved between the event and the run. A verdict written at the old sha
// would be one nobody is looking at, and the newer run covers it.
func TestStaleHeadShaStopsBeforeEvaluating(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{headSHA: "newer999"}
	ev := &event.Event{
		Trigger: event.TriggerPullRequest, Action: "synchronize",
		Owner: "opentalon", Repo: "talooner", PR: 42, HeadSHA: "abc123",
	}

	if err := Run(t.Context(), Runner{Event: ev, GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range f.actions() {
		if a == cluster.ActionEvaluatePR {
			t.Fatal("evaluated at a head sha that has already moved")
		}
	}
}

// --force is parsed and rejected until llm_review lands, and a rejected command
// evaluates nothing.
func TestForceIsRejectedWithoutEvaluating(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review --force"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none", got)
	}
}

func TestRunRejectsAnEventlessRunner(t *testing.T) {
	if err := Run(t.Context(), Runner{}); err == nil {
		t.Fatal("Run = nil error with no event")
	}
}

// The default handle is what an unconfigured repo uses; a run that silently
// answered to something else would answer to nobody.
func TestDefaultHandleIsUsedWhenUnset(t *testing.T) {
	if command.DefaultHandle != "!talooner" {
		t.Fatalf("DefaultHandle = %q", command.DefaultHandle)
	}
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}
	if err := Run(t.Context(), Runner{Event: commentEvent("!TALOONER /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); len(got) != 2 {
		t.Errorf("cluster calls = %v, want subscribe and evaluate", got)
	}
}

// The sticky comment is the human-readable half of the verdict, and the check
// run is the machine-readable one. Both, once, on a run that produced findings.
func TestFindingsArePostedAsOneStickyComment(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "describe your change"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "internal/auth needs a reviewer"},
	)}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Not a create: the manual /review's own acknowledgement already claimed
	// the review topic, so the verdict edits it rather than posting a second
	// comment next to it.
	got := gh.wrote(t)
	if got.created {
		t.Error("posted a second comment instead of editing the acknowledgement")
	}
	if !strings.HasPrefix(got.Body, comment.Marker(comment.TopicReview)) {
		t.Errorf("comment does not start with the review marker:\n%s", got.Body)
	}
	for _, want := range []string{"describe your change", "internal/auth needs a reviewer"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("comment is missing %q:\n%s", want, got.Body)
		}
	}
}

// Thirty pushes, one comment. The marker is what makes the second run find the
// first run's comment.
func TestASecondRunEditsTheSameComment(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "still missing a description"},
	)}
	gh := &fakeGitHub{existing: []existingComment{
		{ID: 77, Body: comment.Marker(comment.TopicReview) + "\nold findings"},
	}}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := gh.wrote(t)
	if got.created {
		t.Fatal("posted a second comment instead of editing the first")
	}
	if got.id != 77 {
		t.Errorf("edited comment %d, want 77", got.id)
	}
	if strings.Contains(got.Body, "old findings") {
		t.Error("the edit kept the previous run's findings, which no longer hold")
	}
}

// Rules that all passed are worth a green check run, not an email to everyone
// watching the PR.
func TestARunWithNothingToSayPostsNoComment(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A manual /review still owes the commander a reply — the acknowledgement
	// it already posted is edited to say there is nothing to report, rather
	// than an automatic push run's silence.
	got := gh.wrote(t)
	if !strings.Contains(got.Body, "Nothing to report") {
		t.Errorf("comment = %q, want it resolved to nothing to report", got.Body)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionSuccess {
		t.Errorf("conclusion = %q, want %q", got, github.ConclusionSuccess)
	}
}

// The findings of the run before this one are now wrong. They are edited to a
// resolved state rather than deleted: the thread under them is somebody's.
func TestFindingsThatNoLongerHoldAreResolvedNotDeleted(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{existing: []existingComment{
		{ID: 77, Body: comment.Marker(comment.TopicReview) + "\nadd a description"},
	}}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := gh.wrote(t)
	if got.created || got.id != 77 {
		t.Fatalf("comment write = %+v, want an edit of 77", got)
	}
	if strings.Contains(got.Body, "add a description") {
		t.Error("the stale finding is still there")
	}
	if !strings.Contains(got.Body, "no longer apply") {
		t.Errorf("the comment was not resolved:\n%s", got.Body)
	}
}

// A ruleset that will not compile gets the annotations and the summary comment
// actions.md pairs them with, on the same topic: it is the current answer to
// the same question.
func TestABrokenRulesetIsExplainedInTheComment(t *testing.T) {
	f := &fakeCluster{
		answers: map[string]proto.Message{
			cluster.ActionSetSubscription: &taloonerpb.SetSubscriptionResponse{Subscribed: true},
			cluster.ActionValidateRuleset: &taloonerpb.ValidateRulesetResponse{},
		},
		failures: map[string]string{cluster.ActionEvaluatePR: "talooner: ruleset does not compile"},
	}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the refusal")
	}

	got := gh.wrote(t)
	if !strings.HasPrefix(got.Body, comment.Marker(comment.TopicReview)) {
		t.Errorf("the broken-ruleset comment is on another topic:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "does not compile") {
		t.Errorf("the comment does not say what broke:\n%s", got.Body)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want %q", got, github.ConclusionNeutral)
	}
}

// One reply to a command Talooner did not understand, on its own topic so a
// typo never overwrites the verdict, and edited rather than piled up.
func TestAnUnknownCommandIsAnsweredOnItsOwnTopic(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /frobnicate"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := gh.wrote(t)
	if !strings.HasPrefix(got.Body, comment.Marker(comment.TopicUsage)) {
		t.Errorf("the reply is not on the usage topic:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "/review") {
		t.Errorf("the reply does not say what the commands are:\n%s", got.Body)
	}
	if len(gh.checks) != 0 {
		t.Errorf("check runs written = %+v, want none: nothing was evaluated", gh.checks)
	}
}

// An unauthorised account gets no reply at all — a reply tells it the bot is
// installed and hands it a way to make the bot post.
func TestAnUnknownCommandFromAnUnauthorisedAccountIsNotAnswered(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{permission: "read"}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /frobnicate"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.comments) != 0 {
		t.Errorf("comments written = %+v, want none", gh.comments)
	}
}

// The comment is written before the check run, so a comment that could not be
// written leaves no verdict standing: the neutral check replaces whatever the
// previous run left, instead of this run's own correct verdict being painted
// over by failOpen.
func TestAFailedCommentLeavesTheCheckNeutral(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "one nit"},
	)}
	gh := &fakeGitHub{commentFails: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the comment failure")
	}
	got := gh.check(t)
	if got.Conclusion != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want %q: a half-written verdict must not read as approved",
			got.Conclusion, github.ConclusionNeutral)
	}
	if len(gh.reviews) != 0 {
		t.Errorf("submitted %+v after the comment write failed", gh.reviews)
	}
}

// verdict returns the one review the run submitted, failing the test if it
// submitted none or more than one.
func (g *fakeGitHub) verdict(t *testing.T) submittedReview {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.reviews) != 1 {
		t.Fatalf("reviews submitted = %d, want exactly 1: %+v", len(g.reviews), g.reviews)
	}
	return g.reviews[0]
}

// standingApproval is what a previous run's approve left on the PR.
// loginsJSON, slugsJSON, assigneesJSON and reviewersJSON are the shapes GitHub
// returns for people: logins for users, slugs for teams.
func loginsJSON(logins []string) string {
	parts := make([]string, 0, len(logins))
	for _, l := range logins {
		parts = append(parts, fmt.Sprintf(`{"login":%q}`, l))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func slugsJSON(slugs []string) string {
	parts := make([]string, 0, len(slugs))
	for _, s := range slugs {
		parts = append(parts, fmt.Sprintf(`{"slug":%q}`, s))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func assigneesJSON(logins []string) string {
	return `{"assignees":` + loginsJSON(logins) + `}`
}

func reviewersJSON(users, teams []string) string {
	return `{"requested_reviewers":` + loginsJSON(users) + `,"requested_teams":` + slugsJSON(teams) + `}`
}

func standingApproval() []existingReview {
	return []existingReview{{ID: 12, Body: review.Marker() + "\napproved", State: github.StateApproved}}
}

func TestApproveSubmitsAReview(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr",
	})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := gh.verdict(t)
	if got.Event != github.ReviewApprove {
		t.Errorf("event = %q, want APPROVE", got.Event)
	}
	if got.CommitID != "abc123" {
		t.Errorf("commit id = %q, want the head sha the verdict was computed from", got.CommitID)
	}
	if !strings.Contains(got.Body, review.Marker()) {
		t.Errorf("body does not carry the marker, so the next run cannot dismiss it: %q", got.Body)
	}
	if len(gh.dismissed) != 0 {
		t.Errorf("dismissed %v with nothing standing", gh.dismissed)
	}
}

// The headline of #18: a PR that was approved and has since grown has its
// approval dismissed, not left standing next to the new request for changes.
func TestBlockDismissesTheEarlierApproval(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr.merge",
	})}
	gh := &fakeGitHub{standing: standingApproval()}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := gh.verdict(t).Event; got != github.ReviewRequestChanges {
		t.Errorf("event = %q, want REQUEST_CHANGES", got)
	}
	if len(gh.dismissed) != 1 || gh.dismissed[0] != "12" {
		t.Errorf("dismissed = %v, want the earlier approval", gh.dismissed)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionFailure {
		t.Errorf("conclusion = %q, want the check run to agree with the review", got)
	}
}

// Retraction is the absence of an action, so no executor is ever handed it: the
// rules stopped approving, and the approval has to come down anyway.
func TestNoVerdictRetractsTheStandingReview(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "describe your change",
	})}
	gh := &fakeGitHub{standing: standingApproval()}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.reviews) != 0 {
		t.Errorf("submitted %+v with no approve or block in the decision", gh.reviews)
	}
	if len(gh.dismissed) != 1 || gh.dismissed[0] != "12" {
		t.Errorf("dismissed = %v, want the approval that no longer holds", gh.dismissed)
	}
}

// A verb nothing here performs — notify until D6 — fails the run before any of
// the verdict is published. Half a verdict on the PR is worse than none, and an
// action silently skipped is the failure nobody notices.
func TestAVerbWithNoExecutorFailsBeforeAnythingIsWritten(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_NOTIFY, Target: "slack.security", Text: "look at this"},
	)}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if err == nil {
		t.Fatal("Run = nil, want the missing executor to fail the run")
	}
	if !errors.Is(err, action.ErrNoExecutor) {
		t.Errorf("err = %v, want ErrNoExecutor", err)
	}
	if verdict := gh.verdictComments(); len(gh.reviews) != 0 || len(verdict) != 0 {
		t.Errorf("published part of the verdict: reviews %+v, comments %+v", gh.reviews, verdict)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral: this is Talooner's own gap, not a policy outcome", got)
	}
}

// The review is written before the check run, so a review that cannot be
// submitted leaves the neutral check rather than a success nothing backs up.
func TestAFailedReviewLeavesTheCheckNeutral(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr",
	})}
	gh := &fakeGitHub{reviewFails: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the review failure")
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral", got)
	}
}

// The assign and require verbs reach GitHub, and what they added is
// recorded so a later run can take it back.
func TestAssignAndRequireAreWrittenAndRecorded(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_ASSIGN, Target: "pr", Assignee: "alice"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_REQUIRE, Target: "review.security"},
	)}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(gh.assignees, "alice") {
		t.Errorf("assignees = %v, want alice", gh.assignees)
	}
	if !slices.Contains(gh.requestedTeams, "security") {
		t.Errorf("requested teams = %v, want security", gh.requestedTeams)
	}

	var ledger string
	for _, c := range gh.comments {
		if strings.Contains(c.Body, comment.Marker(comment.TopicState)) {
			ledger = c.Body
		}
	}
	if ledger == "" {
		t.Fatalf("no ledger comment was written: %+v", gh.comments)
	}
	got, err := assignment.ParseLedger(ledger)
	if err != nil {
		t.Fatalf("ParseLedger: %v", err)
	}
	if len(got.Assignees) != 1 || got.Assignees[0] != "alice" || len(got.Teams) != 1 || got.Teams[0] != "security" {
		t.Errorf("ledger = %+v, want what this run added", got)
	}
}

// Retraction is the absence of an action here too: the rules stopped asking,
// and the assignee has to come off — but only the one Talooner added.
func TestNoActionsRetractsOnlyTaloonersOwnAssignees(t *testing.T) {
	ledger := assignment.LedgerBody(assignment.Ledger{Assignees: []string{"alice"}})
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "describe your change",
	})}
	gh := &fakeGitHub{
		assignees: []string{"alice", "carol"},
		existing:  []existingComment{{ID: 77, Body: comment.Marker(comment.TopicState) + "\n" + ledger}},
	}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"carol"}; !slices.Equal(gh.assignees, want) {
		t.Errorf("assignees = %v, want %v: carol was assigned by a person", gh.assignees, want)
	}
}

// An assignee GitHub silently drops fails the run, and the check run says
// neutral rather than success: Talooner did not do what it reported doing.
func TestAnIgnoredAssigneeLeavesTheCheckNeutral(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_ASSIGN, Target: "pr", Assignee: "stranger",
	})}
	gh := &fakeGitHub{ignored: []string{"stranger"}}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if !errors.Is(err, assignment.ErrIgnored) {
		t.Fatalf("err = %v, want ErrIgnored", err)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral", got)
	}
}

// A require target nothing maps to fails before any part of the verdict is
// published — the same guarantee a missing executor has.
func TestAnUnmappedRequireTargetFailsBeforeAnythingIsWritten(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_REQUIRE, Target: "review.foo.bar"},
	)}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if !errors.Is(err, assignment.ErrTarget) {
		t.Fatalf("err = %v, want ErrTarget", err)
	}
	if verdict := gh.verdictComments(); len(gh.reviews) != 0 || len(verdict) != 0 {
		t.Errorf("published part of the verdict: reviews %+v, comments %+v", gh.reviews, verdict)
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral", got)
	}
}

// E2 (#21): the base ruleset is what governs writes, always. A fork PR's own
// head-branch ruleset is evaluated too, but only in plan mode, and shows up as
// a diff comment against the base decision — never as anything performed.

func TestForkPRPostsTheDecisionDiffAgainstTheBaseRuleset(t *testing.T) {
	f := &fakeCluster{
		answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr"}),
		planAnswer: &taloonerpb.EvaluatePrResponse{
			Plan: []*taloonerpb.Action{{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"}},
		},
	}
	gh := &fakeGitHub{fork: true, headRuleset: "rule \"different\" { }\n"}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var plan string
	for _, c := range gh.comments {
		if strings.Contains(c.Body, comment.Marker(comment.TopicPlan)) {
			plan = c.Body
		}
	}
	if plan == "" {
		t.Fatalf("no plan comment was written: %+v", gh.comments)
	}
	if !strings.Contains(plan, "approve pr") {
		t.Errorf("plan comment does not say the head ruleset would add approve pr:\n%s", plan)
	}
	if !strings.Contains(plan, "block pr") {
		t.Errorf("plan comment does not say the base decision (block pr) would be dropped:\n%s", plan)
	}
}

// The core safety property: a fork's head ruleset approving everything must
// never reach a write, so the count of what was actually submitted has to
// match the base decision alone — asserting on the comment text is not enough.
func TestForkHeadRulesetApprovingEverythingWritesNothingFromIt(t *testing.T) {
	f := &fakeCluster{
		answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr"}),
		planAnswer: &taloonerpb.EvaluatePrResponse{
			Plan: []*taloonerpb.Action{{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"}},
		},
	}
	gh := &fakeGitHub{fork: true, headRuleset: "rule \"approve everything\" { }\n"}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gh.reviews) != 1 || gh.reviews[0].Event != "REQUEST_CHANGES" {
		t.Fatalf("reviews = %+v, want exactly the base decision's REQUEST_CHANGES, nothing from the head ruleset's approve", gh.reviews)
	}
}

func TestForkPRWithNoHeadRulesetWritesNoDiffComment(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{fork: true, noHeadRuleset: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, c := range gh.comments {
		if strings.Contains(c.Body, comment.Marker(comment.TopicPlan)) {
			t.Errorf("a plan comment was written for a fork PR with no head ruleset: %q", c.Body)
		}
	}
}

func TestSameRepoBranchPRHasNoPlanComment(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"})}
	gh := &fakeGitHub{} // fork defaults to false

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, c := range f.calls {
		if c.GetAction() == cluster.ActionEvaluatePR && c.GetArgs()["mode"] == "plan" {
			t.Errorf("evaluate_pr was called in plan mode for a same-repo PR")
		}
	}
	for _, c := range gh.comments {
		if strings.Contains(c.Body, comment.Marker(comment.TopicPlan)) {
			t.Errorf("a plan comment was written for a same-repo PR: %q", c.Body)
		}
	}
}

// A plan comment from an earlier run, now that the head ruleset no longer
// differs from the base decision, is edited to say so rather than left
// describing a diff that no longer holds — same shape as the review comment's
// own Resolved transition.
func TestPlanDiffThatNoLongerHoldsIsResolved(t *testing.T) {
	f := &fakeCluster{
		answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"}),
		planAnswer: &taloonerpb.EvaluatePrResponse{
			Plan: []*taloonerpb.Action{{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"}},
		},
	}
	gh := &fakeGitHub{
		fork:     true,
		existing: []existingComment{{ID: 88, Body: comment.Marker(comment.TopicPlan) + "\nstale diff"}},
	}

	if err := Run(t.Context(), Runner{Event: commentEvent("!talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var edited *writtenComment
	for i, c := range gh.comments {
		if strings.Contains(c.Body, comment.Marker(comment.TopicPlan)) {
			edited = &gh.comments[i]
		}
	}
	if edited == nil {
		t.Fatalf("no plan comment was written: %+v", gh.comments)
	}
	if edited.created || edited.id != 88 {
		t.Errorf("plan comment %+v, want an edit of the existing one (id 88)", edited)
	}
	if !strings.Contains(edited.Body, "no difference") {
		t.Errorf("plan comment does not say the diff is gone:\n%s", edited.Body)
	}
}
