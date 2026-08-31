// Package cluster is the client Talooner makes every plugin call through.
//
// The plugin does not define rpcs of its own. OpenTalon fixes the service in
// opentalon/proto/plugin.proto and a plugin declares *actions*, so every call
// here is PluginService.Execute with an action name and a map of string args,
// and every structured result comes back as JSON in structured_content
// (talooner-plugin/protocol.md, "This is not a custom gRPC service").
//
// Four behaviours are load-bearing rather than incidental:
//
//   - Dial performs the whoami handshake before it returns a client. There is
//     no degraded mode in which the bot reviews without the engine (auth.md,
//     "The run fails fast"), so a client that never handshook cannot exist.
//   - The API key travels in the api_key arg, not as an action parameter, and
//     is registered with the redactor before the first call that could log it.
//   - Every call declares protocol_version. The action version is pinned in a
//     tenant's workflow file and the plugin version is whatever their cluster
//     runs; the handshake is what turns skew into one clear error at the top of
//     a run instead of strange behaviour halfway through an evaluation.
//   - TLS is the default and plaintext needs an explicit http:// host. The
//     endpoint is on the internet and the key is the whole gate, so a typo in
//     OPENTALON_HOST must not silently put the key on the wire in the clear.
//
// The connection is not pooled or reused: a run dials, handshakes, does its
// work and disconnects. Nothing amortises across events.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/github"
	"github.com/opentalon/talooner/internal/version"
)

// Environment the Actions runtime supplies from the tenant's secrets.
const (
	// EnvHost is the cluster endpoint, e.g. grpc://talon.example.com:9090.
	EnvHost = "OPENTALON_HOST"
	// EnvAPIKey is the tenant's cluster API key.
	EnvAPIKey = "OPENTALON_API_KEY"
	// EnvMaxMsgBytes mirrors the cluster-side gRPC receive limit knob. It is an
	// operator setting on the cluster and has to match on both ends, so it is
	// read here too rather than assumed.
	EnvMaxMsgBytes = "OPENTALON_GRPC_MAX_MSG_BYTES"
)

// Wire constants of the plugin contract.
const (
	// PluginName is the name the host registers talooner-plugin under; calls
	// arrive at it as (plugin=PluginName, action=<action>).
	PluginName = "talooner"

	// ActionWhoami is the capability handshake.
	ActionWhoami = "whoami"

	// ArgAPIKey carries the tenant API key. It is a credential, not an action
	// parameter, which is why the plugin does not advertise it.
	ArgAPIKey = "api_key"

	// ArgProtocolVersion is how a caller declares its contract version so the
	// plugin can refuse a below-floor caller at the handshake.
	ArgProtocolVersion = "protocol_version"

	// FeatureLLMReview is the whoami feature that says the cluster has an LLM
	// provider configured. A ruleset using llm_review without it is a warning at
	// load time, not a failure on the first PR.
	FeatureLLMReview = "llm_review"

	// FeatureGenerateRuleset is the whoami feature that says the cluster can
	// scaffold a ruleset via generate_ruleset. Its absence isn't fatal either —
	// the plugin reports source: "fallback" and `onboard` supplies its own
	// starter, so there's nothing to warn about at dial time.
	FeatureGenerateRuleset = "generate_ruleset"
)

// Protocol versions, both directions of the skew.
const (
	// ProtocolVersion is the contract version this build speaks. It is the
	// plugin's own constant rather than a copy: the generated package is the
	// contract, and a hand-maintained duplicate would drift.
	ProtocolVersion = taloonerpb.ProtocolVersion

	// ProtocolFloor is the lowest plugin protocol version this build will talk
	// to. The plugin enforces the mirror image of this against callers; this
	// side catches the case the plugin cannot — a cluster older than the action.
	ProtocolFloor = taloonerpb.ProtocolFloor
)

// defaultMaxMsgBytes is the cluster's default gRPC receive limit (32 MiB,
// opentalon/internal/grpclimit). Assume the default: the override is an
// operator knob and a caller assuming a raised ceiling breaks on a stock
// deployment.
const defaultMaxMsgBytes = 32 << 20

var (
	// ErrMissingHost means OPENTALON_HOST is unset or empty. Distinct from a
	// dial failure: nothing was attempted, the workflow is misconfigured.
	ErrMissingHost = errors.New("OPENTALON_HOST is not set")

	// ErrMissingKey means OPENTALON_API_KEY is unset or empty. Kept distinct
	// from a rejected key so a tenant who forgot the secret is not told to check
	// its value.
	ErrMissingKey = errors.New("OPENTALON_API_KEY is not set")

	// ErrHandshake means whoami did not succeed: unreachable cluster, rejected
	// key, or a plugin that refused this caller. The run stops here — there is
	// no reviewing without the engine.
	ErrHandshake = errors.New("cluster handshake failed")

	// ErrProtocolSkew means the cluster's protocol version is below this
	// build's floor. The other direction — this build below the cluster's floor
	// — is the plugin's error to raise, and it arrives as a handshake failure
	// carrying the plugin's own message naming its floor.
	ErrProtocolSkew = errors.New("cluster protocol version is below this action's floor")

	// ErrAction means the plugin ran and refused: an unknown action, a bad
	// argument, a rate limit. The call reached the engine, so it is not a
	// handshake failure.
	ErrAction = errors.New("plugin action failed")
)

// Client is a connected, authenticated plugin client. The zero value is not
// usable; call Dial.
type Client struct {
	conn     *grpc.ClientConn
	rpc      pluginpb.PluginServiceClient
	apiKey   string
	identity Identity
	log      *slog.Logger
	redactor *github.Redactor
	timeout  time.Duration
	closed   bool

	// Injection seams for the tests; no production caller sets them.
	dialOpts []grpc.DialOption
	callID   func() string
}

// Option configures a Client.
type Option func(*Client)

// WithLogger sets the destination for call logs. The logger is wrapped in a
// redacting handler, so a caller cannot opt out of redaction by passing its own.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

// WithSecrets registers extra values to strip from logs and error messages. The
// API key is registered by Dial.
func WithSecrets(secrets ...string) Option {
	return func(c *Client) { c.redactor = github.NewRedactor(append(secrets, c.apiKey)...) }
}

// WithTimeout caps a single call, the handshake included. It is a whole-call
// budget: WAN latency to someone's VPS, not loopback.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithDialOptions appends gRPC dial options, for a test server or a deployment
// needing custom credentials.
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *Client) { c.dialOpts = append(c.dialOpts, opts...) }
}

// Dial connects to host, authenticates with apiKey and performs the whoami
// handshake. It returns a usable client or an error — never a client that has
// not handshaken, because the alternative is a run that discovers the cluster is
// unreachable halfway through executing actions against someone's PR.
func Dial(ctx context.Context, host, apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(host) == "" {
		return nil, ErrMissingHost
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrMissingKey
	}

	c := &Client{
		apiKey:   apiKey,
		log:      slog.New(slog.DiscardHandler),
		redactor: github.NewRedactor(apiKey),
		timeout:  60 * time.Second,
		callID:   randomCallID,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.log = slog.New(github.RedactHandler(c.log.Handler(), c.redactor))

	target, creds, err := resolve(host)
	if err != nil {
		return nil, err
	}

	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgBytes()),
			grpc.MaxCallSendMsgSize(maxMsgBytes()),
		),
		grpc.WithUserAgent("talooner/" + version.Version),
	}, c.dialOpts...)

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %w", ErrHandshake, target, c.redactor.Error(err))
	}
	c.conn = conn
	c.rpc = pluginpb.NewPluginServiceClient(conn)

	id, err := c.whoami(ctx)
	if err != nil {
		_ = c.conn.Close() //nolint:errcheck // the handshake already failed
		return nil, err
	}
	c.identity = id

	c.log.Info("cluster handshake ok",
		"tenant", id.Tenant, "protocol_version", id.ProtocolVersion,
		"models", len(id.Models), "features", id.Features)
	if !id.HasFeature(FeatureLLMReview) {
		// Not fatal: a ruleset that never calls llm_review runs fine on a
		// cluster with no provider. The ruleset loader turns this into a
		// warning against the actual rules.
		c.log.Warn("cluster reports no llm_review feature, rules using it will not run", "tenant", id.Tenant)
	}
	return c, nil
}

// DialFromEnv builds a client from the two secrets a tenant's workflow sets.
// Options passed here win.
func DialFromEnv(ctx context.Context, opts ...Option) (*Client, error) {
	return Dial(ctx, os.Getenv(EnvHost), os.Getenv(EnvAPIKey), opts...)
}

// Identity returns what whoami reported. It is fixed at dial time: one run, one
// handshake.
func (c *Client) Identity() Identity { return c.identity }

// APIKey returns the cluster key, so a caller can register it with the GitHub
// client's redactor. It is already redacted out of this package's own logs.
func (c *Client) APIKey() string { return c.apiKey }

// Close releases the connection. It is safe to call twice, so a caller can
// defer it and still close early: gRPC reports a second close as a cancelled
// rpc, which is not something a run should fail on.
func (c *Client) Close() error {
	if c.conn == nil || c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// Execute calls one plugin action. args carries the action's own parameters;
// the credential and the protocol version are added here so no call site has to
// remember either. A successful result's structured_content is decoded into out,
// which may be nil for an action whose answer is only its success.
func (c *Client) Execute(ctx context.Context, action string, args map[string]string, out proto.Message) error {
	full := make(map[string]string, len(args)+2)
	for k, v := range args {
		full[k] = v
	}
	full[ArgAPIKey] = c.apiKey
	full[ArgProtocolVersion] = strconv.FormatUint(uint64(ProtocolVersion), 10)

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	resp, err := c.rpc.Execute(ctx, &pluginpb.ToolCallRequest{
		Id:     c.callID(),
		Plugin: PluginName,
		Action: action,
		Args:   full,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", action, c.redactor.Error(err))
	}
	c.log.Debug("plugin action returned", "action", action, "took", time.Since(start).String())

	// The plugin reports a refusal in the error field of a successful rpc: the
	// transport worked, the action did not. Both shapes have to be checked or
	// half the failures read as success.
	if resp.GetError() != "" {
		return fmt.Errorf("%w: %s: %s", ErrAction, action, c.redactor.String(resp.GetError()))
	}
	if out == nil {
		return nil
	}
	raw := resp.GetStructuredContent()
	if raw == "" {
		return fmt.Errorf("%s: plugin returned no structured_content", action)
	}
	// DiscardUnknown: the contract is append-only and a tenant's cluster may be
	// newer than their pinned action, so an added field must not fail a run.
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("%s: decode structured_content: %w", action, c.redactor.Error(err))
	}
	return nil
}

// resolve turns an OPENTALON_HOST value into a gRPC target and its transport
// credentials. Anything but an explicit http:// scheme gets TLS: the host is a
// secret a tenant pasted into a workflow, and a scheme typo must fail closed
// rather than put the API key on the wire in the clear.
func resolve(host string) (string, credentials.TransportCredentials, error) {
	raw := strings.TrimSpace(host)
	if !strings.Contains(raw, "://") {
		return raw, credentials.NewTLS(nil), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s %q: %w", EnvHost, raw, err)
	}
	if u.Host == "" {
		return "", nil, fmt.Errorf("%s %q has no host", EnvHost, raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "grpc", "grpcs", "https", "dns":
		return u.Host, credentials.NewTLS(nil), nil
	case "http":
		// Documented escape hatch for a cluster on the runner's own network and
		// for tests. It is never reached by a default configuration.
		return u.Host, insecure.NewCredentials(), nil
	default:
		return "", nil, fmt.Errorf("%s %q: unsupported scheme %q, want grpc:// or http://", EnvHost, raw, u.Scheme)
	}
}

// maxMsgBytes is the cluster's gRPC receive limit, which has to be set on both
// ends: raising it on one side alone moves the failure to the other direction.
func maxMsgBytes() int {
	if raw := os.Getenv(EnvMaxMsgBytes); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxMsgBytes
}

// randomCallID is the id correlating a request with its response in the
// plugin's logs. It needs to be unique, not unguessable.
func randomCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails as of Go 1.24
	return "talooner-" + hex.EncodeToString(b[:])
}
