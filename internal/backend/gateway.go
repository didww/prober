package backend

import (
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

// Gateway is the gRPC service agents connect to. It authenticates each
// stream by its bearer token, keeps a registry of connected agents, and
// routes job events to whoever is waiting for them.
type Gateway struct {
	pb.UnimplementedAgentGatewayServer

	log          *slog.Logger
	tokens       map[string]string   // pinned token -> site (authoritative)
	sharedToken  string              // enrollment token: any agent may register under the site it declares
	allowedSites map[string]struct{} // when non-empty, constrains shared-token sites

	mu     sync.RWMutex
	agents map[string]*agentConn // site -> connection
	sinks  map[string]EventSink  // job id -> where its events go
}

// EventSink receives an agent's events for one job. The run manager
// implements it.
type EventSink interface {
	OnEvent(*pb.JobEvent)
}

func NewGateway(cfg Config, log *slog.Logger) *Gateway {
	tokens := make(map[string]string, len(cfg.Agents))
	for _, a := range cfg.Agents {
		tokens[a.Token] = a.Site
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedSites))
	for _, s := range cfg.AllowedSites {
		allowed[s] = struct{}{}
	}
	return &Gateway{
		log:          log,
		tokens:       tokens,
		sharedToken:  cfg.AgentToken,
		allowedSites: allowed,
		agents:       make(map[string]*agentConn),
		sinks:        make(map[string]EventSink),
	}
}

// agentConn is one connected agent. send is serialised because gRPC streams
// are not safe for concurrent Send.
type agentConn struct {
	site   string
	hello  *pb.Hello
	stream pb.AgentGateway_SessionServer

	sendMu sync.Mutex
}

func (a *agentConn) send(m *pb.BackendMessage) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return a.stream.Send(m)
}

// bearerToken reads the token from the stream metadata.
func bearerToken(stream pb.AgentGateway_SessionServer) string {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return ""
	}
	if v := md.Get("authorization"); len(v) == 1 {
		const p = "Bearer "
		if len(v[0]) > len(p) && v[0][:len(p)] == p {
			return v[0][len(p):]
		}
	}
	return ""
}

// authSite decides which site a stream may register as, given its token and
// the site it declared in Hello.
//
// A token that matches a pinned per-site token authenticates that site,
// authoritatively — the Hello's claim is ignored. Otherwise, if a shared
// enrollment token is configured and matches, the agent registers under the
// site it declared (autoregistration), optionally constrained to allowed_sites.
// All comparisons are constant time, so a wrong token cannot be told from
// another by timing.
func (g *Gateway) authSite(tok string, hello *pb.Hello) (string, error) {
	if tok == "" {
		return "", status.Error(codes.Unauthenticated, "missing bearer token")
	}
	pinned := ""
	for known, s := range g.tokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(known)) == 1 {
			pinned = s
		}
	}
	if pinned != "" {
		return pinned, nil
	}
	if g.sharedToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(g.sharedToken)) == 1 {
		site := hello.GetSite()
		if site == "" {
			return "", status.Error(codes.InvalidArgument, "hello.site is required to register with the shared token")
		}
		if len(g.allowedSites) > 0 {
			if _, ok := g.allowedSites[site]; !ok {
				return "", status.Errorf(codes.PermissionDenied, "site %q is not in allowed_sites", site)
			}
		}
		return site, nil
	}
	return "", status.Error(codes.Unauthenticated, "unknown token")
}

func (g *Gateway) Session(stream pb.AgentGateway_SessionServer) error {
	tok := bearerToken(stream)

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}

	// The site comes from the token (pinned) or, with the shared token, from
	// the agent's own declaration.
	site, err := g.authSite(tok, hello)
	if err != nil {
		return err
	}

	conn := &agentConn{site: site, hello: hello, stream: stream}
	g.addAgent(conn)
	defer g.removeAgent(site, conn)

	g.log.Info("agent connected", "site", site, "version", hello.Version, "claimed_site", hello.Site)

	if err := conn.send(&pb.BackendMessage{Msg: &pb.BackendMessage_Welcome{Welcome: &pb.Welcome{
		Site:                site,
		Time:                timestamppb.Now(),
		HeartbeatIntervalMs: 15000,
	}}}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			g.log.Info("agent stream closed", "site", site, "err", err)
			return err
		}
		switch m := msg.Msg.(type) {
		case *pb.AgentMessage_Event:
			g.route(m.Event)
		case *pb.AgentMessage_Heartbeat:
			// Liveness only; connection state is the registry.
		case *pb.AgentMessage_Hello:
			// Ignore a second Hello.
		}
	}
}

func (g *Gateway) addAgent(c *agentConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if old := g.agents[c.site]; old != nil {
		// A new stream for a site replaces the old one; the old Session
		// returns when its Recv fails.
		g.log.Warn("agent reconnected, replacing previous stream", "site", c.site)
	}
	g.agents[c.site] = c
}

func (g *Gateway) removeAgent(site string, c *agentConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.agents[site] == c {
		delete(g.agents, site)
		g.log.Info("agent disconnected", "site", site)
	}
}

func (g *Gateway) route(ev *pb.JobEvent) {
	g.mu.RLock()
	sink := g.sinks[ev.JobId]
	g.mu.RUnlock()
	if sink != nil {
		sink.OnEvent(ev)
	}
}

// Sites returns the connected sites and their Hello, for the /api/probers list.
func (g *Gateway) Sites() []*pb.Hello {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*pb.Hello, 0, len(g.agents))
	for _, c := range g.agents {
		out = append(out, c.hello)
	}
	return out
}

// Connected reports whether a site has a live agent.
func (g *Gateway) Connected(site string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.agents[site] != nil
}

// StartJob dispatches a job to a site's agent and registers where its events
// go. It fails if the site has no connected agent.
func (g *Gateway) StartJob(site, jobID string, spec *pb.TraceSpec, sink EventSink) error {
	g.mu.Lock()
	conn := g.agents[site]
	if conn == nil {
		g.mu.Unlock()
		return errors.New("site not connected")
	}
	g.sinks[jobID] = sink
	g.mu.Unlock()

	return conn.send(&pb.BackendMessage{Msg: &pb.BackendMessage_Start{Start: &pb.StartJob{
		JobId: jobID,
		Spec:  &pb.StartJob_Trace{Trace: spec},
	}}})
}

// CancelJob asks a site's agent to stop a job.
func (g *Gateway) CancelJob(site, jobID string) {
	g.mu.RLock()
	conn := g.agents[site]
	g.mu.RUnlock()
	if conn != nil {
		_ = conn.send(&pb.BackendMessage{Msg: &pb.BackendMessage_Cancel{Cancel: &pb.CancelJob{JobId: jobID}}})
	}
}

// EndJob removes a job's sink once the run no longer needs it.
func (g *Gateway) EndJob(jobID string) {
	g.mu.Lock()
	delete(g.sinks, jobID)
	g.mu.Unlock()
}
