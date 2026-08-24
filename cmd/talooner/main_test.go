package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/credentials"
)

const testKey = "otk_test_0123456789abcdef"

// fakePlugin answers every Execute call from a fixed script, mirroring
// internal/cluster's own test double — this package can't import that one
// (unexported, different package), and the CLI's job is exercising Dial's
// error paths end to end, not just the credentials/flag plumbing around it.
type fakePlugin struct {
	pluginpb.UnimplementedPluginServiceServer
	respond func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse
}

func (f *fakePlugin) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	return f.respond(req), nil
}

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

func whoamiOK(t *testing.T) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	t.Helper()
	return structured(t, &taloonerpb.WhoamiResponse{
		Tenant:          "acme",
		ProtocolVersion: taloonerpb.ProtocolVersion,
		Models:          []string{"claude-opus"},
		Features:        []string{"llm_review"},
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

// withHome points $HOME (and so credentials.DefaultPath) at a scratch
// directory for the duration of the test.
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(context.Background(), nil, &out, &errw)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "Usage:") {
		t.Errorf("stderr = %q, want usage text", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"bogus"}, &out, &errw)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), `"bogus"`) {
		t.Errorf("stderr = %q, want it to name the bad command", errw.String())
	}
}

func TestClusterLoginMissingFlags(t *testing.T) {
	withHome(t)
	cases := [][]string{
		{"cluster", "login"},
		{"cluster", "login", "--url", "grpc://x:9090"},
		{"cluster", "login", "--key", "otk_x"},
	}
	for _, args := range cases {
		var out, errw bytes.Buffer
		code := run(context.Background(), args, &out, &errw)
		if code != 2 {
			t.Errorf("run(%v) code = %d, want 2", args, code)
		}
		if out.Len() != 0 {
			t.Errorf("run(%v) stdout = %q, want empty", args, out.String())
		}
	}
}

func TestClusterLoginNeverEchoesKey(t *testing.T) {
	withHome(t)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"cluster", "login", "--url", "grpc://x:9090", "--key", testKey}, &out, &errw)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, errw.String())
	}
	if strings.Contains(out.String(), testKey) {
		t.Errorf("stdout = %q, must never contain the API key", out.String())
	}
	if strings.Contains(errw.String(), testKey) {
		t.Errorf("stderr = %q, must never contain the API key", errw.String())
	}
}

func TestClusterLoginSavesWithoutDialing(t *testing.T) {
	home := withHome(t)
	var out, errw bytes.Buffer
	// No cluster listening anywhere: login must still succeed. Dialing is
	// whoami's job.
	code := run(context.Background(), []string{"cluster", "login", "--url", "grpc://127.0.0.1:1", "--key", testKey}, &out, &errw)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, errw.String())
	}

	creds, err := credentials.Load(filepath.Join(home, ".talooner", "credentials"))
	if err != nil {
		t.Fatalf("Load saved credentials: %v", err)
	}
	if creds.Host != "grpc://127.0.0.1:1" || creds.APIKey != testKey {
		t.Errorf("saved credentials = %+v, want the flags round-tripped", creds)
	}
}

func TestClusterWhoamiNoStoredCredentials(t *testing.T) {
	withHome(t)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"cluster", "whoami"}, &out, &errw)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at `cluster login`", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestClusterWhoamiSuccess(t *testing.T) {
	home := withHome(t)
	host := serve(t, &fakePlugin{respond: whoamiOK(t)})
	if err := credentials.Save(filepath.Join(home, ".talooner", "credentials"), credentials.Credentials{Host: host, APIKey: testKey}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"cluster", "whoami"}, &out, &errw)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, errw.String())
	}
	for _, want := range []string{"tenant:", "acme", "protocol_version:", "quota:", "12 / 100"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", out.String(), want)
		}
	}
}

func TestClusterWhoamiRejectedKey(t *testing.T) {
	home := withHome(t)
	host := serve(t, &fakePlugin{respond: failWith("invalid api key")})
	if err := credentials.Save(filepath.Join(home, ".talooner", "credentials"), credentials.Credentials{Host: host, APIKey: testKey}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"cluster", "whoami"}, &out, &errw)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "rejected by cluster") {
		t.Errorf("stderr = %q, want it to say the key was rejected, not that the cluster is unreachable", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestClusterWhoamiUnreachable(t *testing.T) {
	home := withHome(t)
	// Nothing listens on port 1: connection refused, no server involved.
	if err := credentials.Save(filepath.Join(home, ".talooner", "credentials"), credentials.Credentials{Host: "http://127.0.0.1:1", APIKey: testKey}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"cluster", "whoami"}, &out, &errw)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cannot reach cluster") {
		t.Errorf("stderr = %q, want the unreachable-host message", errw.String())
	}
	if strings.Contains(errw.String(), "rejected by cluster") {
		t.Errorf("stderr = %q, unreachable host must not read as a rejected key", errw.String())
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"version"}, &out, &errw)
	if code != 0 {
		t.Fatalf("run: code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "talooner") {
		t.Errorf("stdout = %q, want it to name the binary", out.String())
	}
}
