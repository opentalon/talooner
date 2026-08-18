package cluster

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

const testKey = "otk_test_0123456789abcdef"

// fakePlugin is a PluginService that records what it was called with and
// answers from a per-action script.
type fakePlugin struct {
	pluginpb.UnimplementedPluginServiceServer

	respond func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse

	calls []*pluginpb.ToolCallRequest
}

func (f *fakePlugin) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	f.calls = append(f.calls, req)
	return f.respond(req), nil
}

// serve starts f on a loopback listener and returns an http:// host for it.
func serve(t *testing.T, f *fakePlugin) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }() //nolint:errcheck // stopped by the cleanup below
	t.Cleanup(srv.Stop)
	return "http://" + lis.Addr().String()
}

// whoamiOK answers every call with a healthy handshake.
func whoamiOK(t *testing.T) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	t.Helper()
	return structured(t, &taloonerpb.WhoamiResponse{
		Tenant:          "acme",
		ProtocolVersion: taloonerpb.ProtocolVersion,
		Models:          []string{"claude-opus"},
		Features:        []string{FeatureLLMReview},
		Quota:           &taloonerpb.Quota{LlmCallsUsed: 12, LlmCallsLimit: 100},
	})
}

func structured(t *testing.T, msg proto.Message) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	t.Helper()
	raw, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		return &pluginpb.ToolResultResponse{CallId: req.GetId(), StructuredContent: string(raw)}
	}
}

func failWith(msg string) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	return func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		return &pluginpb.ToolResultResponse{CallId: req.GetId(), Error: msg}
	}
}

func dialFake(t *testing.T, f *fakePlugin, opts ...Option) (*Client, error) {
	t.Helper()
	host := serve(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, host, testKey, opts...)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() }) //nolint:errcheck // test teardown
	}
	return c, err
}

func TestDialHandshake(t *testing.T) {
	f := &fakePlugin{respond: whoamiOK(t)}
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	id := c.Identity()
	if id.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", id.Tenant)
	}
	if id.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol version = %d, want %d", id.ProtocolVersion, ProtocolVersion)
	}
	if !id.HasFeature(FeatureLLMReview) {
		t.Errorf("features = %v, want llm_review", id.Features)
	}
	if id.HasFeature("dispatch") {
		t.Error("HasFeature(dispatch) = true, want false for an unadvertised feature")
	}
	if !id.HasModel("claude-opus") || id.HasModel("gpt-4") {
		t.Errorf("models = %v", id.Models)
	}
	if id.Quota.LLMCallsUsed != 12 || id.Quota.LLMCallsLimit != 100 {
		t.Errorf("quota = %+v, want {12 100}", id.Quota)
	}

	if len(f.calls) != 1 {
		t.Fatalf("plugin saw %d calls, want 1", len(f.calls))
	}
	call := f.calls[0]
	if call.GetPlugin() != PluginName || call.GetAction() != ActionWhoami {
		t.Errorf("call = (%q, %q), want (%q, %q)", call.GetPlugin(), call.GetAction(), PluginName, ActionWhoami)
	}
	if got := call.GetArgs()[ArgAPIKey]; got != testKey {
		t.Errorf("api_key arg = %q, want the key", got)
	}
	want := strconv.FormatUint(uint64(ProtocolVersion), 10)
	if got := call.GetArgs()[ArgProtocolVersion]; got != want {
		t.Errorf("protocol_version arg = %q, want %q", got, want)
	}
	if call.GetId() == "" {
		t.Error("call id is empty, the plugin cannot correlate the call in its logs")
	}
}

// The three failures the ticket is about have to stay three failures: a tenant
// who forgot a secret must not be told to check the value of the one they did
// set, and neither must read as "the cluster is down".
func TestDialMissingConfig(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		key     string
		wantErr error
	}{
		{"no host", "", testKey, ErrMissingHost},
		{"blank host", "   ", testKey, ErrMissingHost},
		{"no key", "http://127.0.0.1:1", "", ErrMissingKey},
		{"blank key", "http://127.0.0.1:1", "  ", ErrMissingKey},
		{"neither", "", "", ErrMissingHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Dial(t.Context(), tt.host, tt.key)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if c != nil {
				t.Error("Dial returned a client alongside an error")
			}
		})
	}
}

func TestDialRejectedKey(t *testing.T) {
	f := &fakePlugin{respond: failWith("talooner: authentication failed")}
	c, err := dialFake(t, f)
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("err = %v, want ErrHandshake", err)
	}
	if c != nil {
		t.Fatal("Dial returned a client for a rejected key; there is no degraded mode")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("err = %v, want the plugin's own message", err)
	}
}

// The plugin refusing a below-floor caller arrives as a handshake failure
// carrying its message, which names the floor to upgrade past. That message is
// the whole value of the error, so it must survive to the top of the run.
func TestDialCallerBelowPluginFloor(t *testing.T) {
	f := &fakePlugin{respond: failWith(
		"talooner: caller protocol version 1 is below this plugin's floor 2; upgrade the talooner action")}
	_, err := dialFake(t, f)
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("err = %v, want ErrHandshake", err)
	}
	if !strings.Contains(err.Error(), "floor 2") {
		t.Errorf("err = %v, want the floor named", err)
	}
}

// The other direction: a cluster older than this action. The plugin cannot
// detect it — it only sees a caller from the future — so this side must.
func TestDialClusterBelowFloor(t *testing.T) {
	f := &fakePlugin{respond: structured(t, &taloonerpb.WhoamiResponse{
		Tenant:          "acme",
		ProtocolVersion: ProtocolFloor - 1,
	})}
	c, err := dialFake(t, f)
	if !errors.Is(err, ErrProtocolSkew) {
		t.Fatalf("err = %v, want ErrProtocolSkew", err)
	}
	if c != nil {
		t.Fatal("Dial returned a client against a below-floor cluster")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(int(ProtocolFloor))) {
		t.Errorf("err = %v, want the required floor named", err)
	}
}

// protojson omits a zero-valued field, so a plugin that never set
// protocol_version is indistinguishable on the wire from one reporting 0. Both
// are below the floor, and neither may pass as "close enough".
func TestDialAbsentProtocolVersion(t *testing.T) {
	f := &fakePlugin{respond: structured(t, &taloonerpb.WhoamiResponse{Tenant: "acme"})}
	if _, err := dialFake(t, f); !errors.Is(err, ErrProtocolSkew) {
		t.Fatalf("err = %v, want ErrProtocolSkew", err)
	}
}

// A response that decodes but names no tenant means the handshake landed on
// something that is not this plugin. Reading that as success would let the run
// continue against an endpoint that will fail every later call.
func TestDialEmptyTenant(t *testing.T) {
	f := &fakePlugin{respond: structured(t, &taloonerpb.WhoamiResponse{
		ProtocolVersion: taloonerpb.ProtocolVersion,
	})}
	_, err := dialFake(t, f)
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("err = %v, want ErrHandshake", err)
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("err = %v, want the missing tenant named", err)
	}
}

func TestDialMalformedResponse(t *testing.T) {
	tests := []struct {
		name string
		resp *pluginpb.ToolResultResponse
		want string
	}{
		{"no structured content", &pluginpb.ToolResultResponse{Content: "tenant=acme"}, "structured_content"},
		{"not json", &pluginpb.ToolResultResponse{StructuredContent: "tenant=acme"}, "decode"},
		{"wrong shape", &pluginpb.ToolResultResponse{StructuredContent: `{"tenant": 42}`}, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakePlugin{respond: func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
				return tt.resp
			}}
			_, err := dialFake(t, f)
			if !errors.Is(err, ErrHandshake) {
				t.Fatalf("err = %v, want ErrHandshake", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A field the cluster added after this action was pinned must not fail a run:
// the contract is append-only and tenants upgrade the two halves separately.
func TestDialIgnoresUnknownFields(t *testing.T) {
	f := &fakePlugin{respond: func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		return &pluginpb.ToolResultResponse{
			CallId:            req.GetId(),
			StructuredContent: `{"tenant":"acme","protocolVersion":1,"regionsFromTheFuture":["eu"]}`,
		}
	}}
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c.Identity().Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", c.Identity().Tenant)
	}
}

func TestDialUnreachable(t *testing.T) {
	// Port 1 on loopback: nothing listens, and the connection is refused rather
	// than left hanging.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, "http://127.0.0.1:1", testKey, WithTimeout(2*time.Second))
	if !errors.Is(err, ErrHandshake) {
		t.Fatalf("err = %v, want ErrHandshake", err)
	}
	if c != nil {
		t.Fatal("Dial returned a client against a dead endpoint")
	}
}

// The key is in every call's args, so it is in reach of every log line the
// client writes. Redaction is at the handler for exactly that reason.
func TestLogsRedactTheKey(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	f := &fakePlugin{respond: failWith("rejected key " + testKey)}
	_, err := dialFake(t, f, WithLogger(log))
	if err == nil {
		t.Fatal("Dial succeeded, want the scripted failure")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error carries the api key: %v", err)
	}

	f2 := &fakePlugin{respond: whoamiOK(t)}
	c, err := dialFake(t, f2, WithLogger(log))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Execute(t.Context(), ActionWhoami, map[string]string{"repo": "acme/api"}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(buf.String(), testKey) {
		t.Errorf("log carries the api key:\n%s", buf.String())
	}
}

// A caller's own arg must never be able to overwrite the credential or the
// declared protocol version.
func TestExecuteArgsCannotOverrideCredentials(t *testing.T) {
	f := &fakePlugin{respond: whoamiOK(t)}
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	args := map[string]string{ArgAPIKey: "attacker", ArgProtocolVersion: "999", "repo": "acme/api"}
	if err := c.Execute(t.Context(), "is_subscribed", args, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := f.calls[len(f.calls)-1].GetArgs()
	if got[ArgAPIKey] != testKey {
		t.Errorf("api_key arg = %q, want the client's key", got[ArgAPIKey])
	}
	if got[ArgProtocolVersion] != strconv.FormatUint(uint64(ProtocolVersion), 10) {
		t.Errorf("protocol_version arg = %q, want this build's", got[ArgProtocolVersion])
	}
	if got["repo"] != "acme/api" {
		t.Errorf("repo arg = %q, want it passed through", got["repo"])
	}
	if _, ok := args[ArgAPIKey]; !ok || args[ArgAPIKey] != "attacker" {
		t.Error("Execute mutated the caller's args map")
	}
}

func TestExecuteActionError(t *testing.T) {
	f := &fakePlugin{respond: whoamiOK(t)}
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	f.respond = failWith(`talooner: unknown action "evaluate_prr"`)

	err = c.Execute(t.Context(), "evaluate_prr", nil, &taloonerpb.WhoamiResponse{})
	if !errors.Is(err, ErrAction) {
		t.Fatalf("err = %v, want ErrAction", err)
	}
	if errors.Is(err, ErrHandshake) {
		t.Error("an action failure must not read as a handshake failure")
	}
}

func TestExecuteRespectsTimeout(t *testing.T) {
	f := &fakePlugin{respond: whoamiOK(t)}
	c, err := dialFake(t, f, WithTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	f.respond = func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		<-done
		return &pluginpb.ToolResultResponse{}
	}
	if err := c.Execute(t.Context(), "evaluate_pr", nil, nil); err == nil {
		t.Fatal("Execute returned nil against a hung plugin")
	}
}

func TestDialFromEnv(t *testing.T) {
	f := &fakePlugin{respond: whoamiOK(t)}
	t.Setenv(EnvHost, serve(t, f))
	t.Setenv(EnvAPIKey, testKey)

	c, err := DialFromEnv(t.Context())
	if err != nil {
		t.Fatalf("DialFromEnv: %v", err)
	}
	defer c.Close() //nolint:errcheck // test teardown
	if c.Identity().Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", c.Identity().Tenant)
	}
	if c.APIKey() != testKey {
		t.Errorf("APIKey() = %q, want the env key", c.APIKey())
	}
}

func TestDialFromEnvMissingSecrets(t *testing.T) {
	t.Setenv(EnvHost, "")
	t.Setenv(EnvAPIKey, testKey)
	if _, err := DialFromEnv(t.Context()); !errors.Is(err, ErrMissingHost) {
		t.Fatalf("err = %v, want ErrMissingHost", err)
	}

	t.Setenv(EnvHost, "grpc://cluster.example.com:9090")
	t.Setenv(EnvAPIKey, "")
	if _, err := DialFromEnv(t.Context()); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
}

// TLS is the default and only http:// opts out. A scheme typo must fail rather
// than downgrade the transport carrying the key.
func TestResolveHost(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		wantTarget string
		wantTLS    bool
		wantErr    bool
	}{
		{name: "bare host port", host: "cluster.example.com:9090", wantTarget: "cluster.example.com:9090", wantTLS: true},
		{name: "grpc scheme", host: "grpc://cluster.example.com:9090", wantTarget: "cluster.example.com:9090", wantTLS: true},
		{name: "grpcs scheme", host: "grpcs://cluster.example.com:9090", wantTarget: "cluster.example.com:9090", wantTLS: true},
		{name: "https scheme", host: "https://cluster.example.com", wantTarget: "cluster.example.com", wantTLS: true},
		{name: "surrounding space", host: "  grpc://cluster.example.com:9090 ", wantTarget: "cluster.example.com:9090", wantTLS: true},
		{name: "http opts out", host: "http://127.0.0.1:9090", wantTarget: "127.0.0.1:9090", wantTLS: false},
		{name: "unknown scheme", host: "ftp://cluster.example.com", wantErr: true},
		{name: "scheme without host", host: "grpc://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, creds, err := resolve(tt.host)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolve(%q) = %q, want an error", tt.host, target)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%q): %v", tt.host, err)
			}
			if target != tt.wantTarget {
				t.Errorf("target = %q, want %q", target, tt.wantTarget)
			}
			if secure := creds.Info().SecurityProtocol != "insecure"; secure != tt.wantTLS {
				t.Errorf("security = %q, want tls = %v", creds.Info().SecurityProtocol, tt.wantTLS)
			}
		})
	}
}

func TestMaxMsgBytes(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		{"unset assumes the cluster default", "", defaultMaxMsgBytes},
		{"override", "1048576", 1048576},
		{"garbage falls back", "banana", defaultMaxMsgBytes},
		{"zero falls back", "0", defaultMaxMsgBytes},
		{"negative falls back", "-1", defaultMaxMsgBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvMaxMsgBytes, tt.env)
			if got := maxMsgBytes(); got != tt.want {
				t.Errorf("maxMsgBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCallIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := randomCallID()
		if seen[id] {
			t.Fatalf("duplicate call id %q", id)
		}
		seen[id] = true
	}
}

func TestCloseIsSafeTwice(t *testing.T) {
	f := &fakePlugin{respond: whoamiOK(t)}
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
