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

const (
	EnvHost        = "OPENTALON_HOST"
	EnvAPIKey      = "OPENTALON_API_KEY"
	EnvMaxMsgBytes = "OPENTALON_GRPC_MAX_MSG_BYTES"
)

const (
	PluginName = "talooner"

	ActionWhoami = "whoami"

	ArgAPIKey = "api_key"

	ArgProtocolVersion = "protocol_version"

	FeatureLLMReview = "llm_review"

	FeatureGenerateRuleset = "generate_ruleset"
)

const (
	ProtocolVersion = taloonerpb.ProtocolVersion

	ProtocolFloor = taloonerpb.ProtocolFloor
)

const defaultMaxMsgBytes = 32 << 20

var (
	ErrMissingHost = errors.New("OPENTALON_HOST is not set")

	ErrMissingKey = errors.New("OPENTALON_API_KEY is not set")

	ErrHandshake = errors.New("cluster handshake failed")

	ErrProtocolSkew = errors.New("cluster protocol version is below this action's floor")

	ErrAction = errors.New("plugin action failed")
)

type Client struct {
	conn     *grpc.ClientConn
	rpc      pluginpb.PluginServiceClient
	apiKey   string
	identity Identity
	log      *slog.Logger
	redactor *github.Redactor
	timeout  time.Duration
	closed   bool

	dialOpts []grpc.DialOption
	callID   func() string
}

type Option func(*Client)

func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

func WithSecrets(secrets ...string) Option {
	return func(c *Client) { c.redactor = github.NewRedactor(append(secrets, c.apiKey)...) }
}

func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *Client) { c.dialOpts = append(c.dialOpts, opts...) }
}

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
		_ = c.conn.Close() //nolint:errcheck
		return nil, err
	}
	c.identity = id

	c.log.Info("cluster handshake ok",
		"tenant", id.Tenant, "protocol_version", id.ProtocolVersion,
		"models", len(id.Models), "features", id.Features)
	if !id.HasFeature(FeatureLLMReview) {
		c.log.Warn("cluster reports no llm_review feature, rules using it will not run", "tenant", id.Tenant)
	}
	return c, nil
}

func DialFromEnv(ctx context.Context, opts ...Option) (*Client, error) {
	return Dial(ctx, os.Getenv(EnvHost), os.Getenv(EnvAPIKey), opts...)
}

func (c *Client) Identity() Identity { return c.identity }

func (c *Client) APIKey() string { return c.apiKey }

func (c *Client) Close() error {
	if c.conn == nil || c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

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
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("%s: decode structured_content: %w", action, c.redactor.Error(err))
	}
	return nil
}

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
		return u.Host, insecure.NewCredentials(), nil
	default:
		return "", nil, fmt.Errorf("%s %q: unsupported scheme %q, want grpc:// or http://", EnvHost, raw, u.Scheme)
	}
}

func maxMsgBytes() int {
	if raw := os.Getenv(EnvMaxMsgBytes); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxMsgBytes
}

func randomCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck
	return "talooner-" + hex.EncodeToString(b[:])
}
