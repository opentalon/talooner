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
}

func (f *fakeCluster) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	if msg, ok := f.failures[req.GetAction()]; ok {
		return &pluginpb.ToolResultResponse{CallId: req.GetId(), Error: msg}, nil
	}
	answer, ok := f.answers[req.GetAction()]
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
	checks   []writtenCheck
	comments []writtenComment

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
}

// existingReview is a review already on the PR when the run starts.
type existingReview struct {
	ID    int64  `json:"id"`
	Body  string `json:"body"`
	State string `json:"state"`
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
			raw, err := json.Marshal(g.existing)
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
			if !got.created {
				id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Sscan(id, &got.id) //nolint:errcheck // a bad id fails the assertions below
			}
			g.mu.Lock()
			g.comments = append(g.comments, got)
			g.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"id":555}`)

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

		case strings.HasSuffix(r.URL.Path, "/permission"):
			_, _ = fmt.Fprintf(w, `{"permission":%q}`, g.permission)

		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/rules.tln"):
			if g.noRuleset {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(ruleset), base64.StdEncoding.EncodeToString([]byte(ruleset)))

		case strings.HasSuffix(r.URL.Path, "/files"):
			if g.failFiles {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			_, _ = fmt.Fprint(w, `[{"filename":"internal/auth/token.go"}]`)

		default: // the pull request itself
			if g.prStatus != 0 {
				w.WriteHeader(g.prStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{
				"number": 42,
				"head": {"sha": %q, "ref": "feat/x", "repo": {"full_name": "opentalon/talooner"}},
				"base": {"sha": "def456", "ref": "master", "repo": {"full_name": "opentalon/talooner"}},
				"user": {"login": "evgeny"},
				"title": "Add a thing", "body": "", "state": "open",
				"additions": 10, "deletions": 3, "changed_files": 1, "commits": 2
			}`, g.headSHA)
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

// wrote returns the one comment the run wrote, failing the test if it wrote
// none or more than one.
func (g *fakeGitHub) wrote(t *testing.T) writtenComment {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.comments) != 1 {
		t.Fatalf("comments written = %d, want exactly 1: %+v", len(g.comments), g.comments)
	}
	return g.comments[0]
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := gh.check(t); got.Conclusion != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral", got.Conclusion)
	}
}

// A repo that has not onboarded gets no check at all: a neutral talooner check
// on a repo that never asked for one is noise.
func TestMissingRulesetWritesNoCheckRun(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{noRuleset: true}

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.checks) != 0 {
		t.Errorf("check runs written = %+v, want none", gh.checks)
	}
}

// The hardest requirement of D2: Talooner's own faults are neutral. A repo that
// marked the check required must not be blocked because the bot broke.
func TestABrokenRunWritesNeutralNotFailure(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_UNSPECIFIED})}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
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

	err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh, Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want an error when the permission check breaks")
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none", got)
	}
}

func TestUnknownCommandFromAnAuthorizedUserEvaluatesNothing(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /shipit"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /stop"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review --force"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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
	if command.DefaultHandle != "@talooner" {
		t.Fatalf("DefaultHandle = %q", command.DefaultHandle)
	}
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}
	if err := Run(t.Context(), Runner{Event: commentEvent("@TALOONER /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := gh.wrote(t)
	if !got.created {
		t.Error("edited a comment on a PR that had none")
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.comments) != 0 {
		t.Errorf("comments written = %+v, want none", gh.comments)
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /frobnicate"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /frobnicate"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
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
func standingApproval() []existingReview {
	return []existingReview{{ID: 12, Body: review.Marker() + "\napproved", State: github.StateApproved}}
}

func TestApproveSubmitsAReview(t *testing.T) {
	f := &fakeCluster{answers: evaluated(&taloonerpb.Action{
		Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr",
	})}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gh.reviews) != 0 {
		t.Errorf("submitted %+v with no approve or block in the decision", gh.reviews)
	}
	if len(gh.dismissed) != 1 || gh.dismissed[0] != "12" {
		t.Errorf("dismissed = %v, want the approval that no longer holds", gh.dismissed)
	}
}

// A verb nothing here performs — assign until D5, notify until D6 — fails the
// run before any of the verdict is published. Half a verdict on the PR is worse
// than none, and an action silently skipped is the failure nobody notices.
func TestAVerbWithNoExecutorFailsBeforeAnythingIsWritten(t *testing.T) {
	f := &fakeCluster{answers: evaluated(
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"},
		&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_ASSIGN, Target: "pr", Assignee: "alice"},
	)}
	gh := &fakeGitHub{}

	err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
	if err == nil {
		t.Fatal("Run = nil, want the missing executor to fail the run")
	}
	if !errors.Is(err, action.ErrNoExecutor) {
		t.Errorf("err = %v, want ErrNoExecutor", err)
	}
	if len(gh.reviews) != 0 || len(gh.comments) != 0 {
		t.Errorf("published part of the verdict: reviews %+v, comments %+v", gh.reviews, gh.comments)
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

	if err := Run(t.Context(), Runner{Event: commentEvent("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want the review failure")
	}
	if got := gh.check(t).Conclusion; got != github.ConclusionNeutral {
		t.Errorf("conclusion = %q, want neutral", got)
	}
}
