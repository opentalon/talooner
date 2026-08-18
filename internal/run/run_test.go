package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/command"
	"github.com/opentalon/talooner/internal/event"
	"github.com/opentalon/talooner/internal/github"
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

// fakeGitHub serves the four endpoints a run reads and records the paths hit.
type fakeGitHub struct {
	mu    sync.Mutex
	paths []string

	permission string // the collaborator permission level to report
	prStatus   int    // non-zero to fail the pull request fetch
	noRuleset  bool
	headSHA    string
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

func comment(body string) *event.Event {
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

	if err := Run(t.Context(), Runner{Event: comment("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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
func TestCommentWithNoCommandTouchesNothing(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: comment("looks good to me"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: comment("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: comment("@talooner /review"), GitHub: gh, Cluster: dialFake(t, f)}); err == nil {
		t.Fatal("Run = nil, want an error when the permission check breaks")
	}
	if got := f.actions(); len(got) != 0 {
		t.Errorf("cluster calls = %v, want none", got)
	}
}

func TestUnknownCommandFromAnAuthorizedUserEvaluatesNothing(t *testing.T) {
	f := &fakeCluster{answers: evaluated()}
	gh := &fakeGitHub{}

	if err := Run(t.Context(), Runner{Event: comment("@talooner /shipit"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: comment("@talooner /stop"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	if err := Run(t.Context(), Runner{Event: comment("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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

	err := Run(t.Context(), Runner{Event: comment("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	err := Run(t.Context(), Runner{Event: comment("@talooner /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)})
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

	if err := Run(t.Context(), Runner{Event: comment("@talooner /review --force"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
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
	if err := Run(t.Context(), Runner{Event: comment("@TALOONER /review"), GitHub: gh.client(t), Cluster: dialFake(t, f)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.actions(); len(got) != 2 {
		t.Errorf("cluster calls = %v, want subscribe and evaluate", got)
	}
}
