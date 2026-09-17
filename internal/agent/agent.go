package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/didww/prober/api/gen/prober/v1"
	"github.com/didww/prober/internal/trace"
)

// Config is the whole agent configuration.
type Config struct {
	Backend BackendConfig `yaml:"backend"`
	Limits  Limits        `yaml:"limits"`
}

// Limits bound what this agent will run, and are reported to the backend in
// Hello so the backend can clamp before dispatching.
type Limits struct {
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
	MaxCycles         int `yaml:"max_cycles"`
	MinIntervalMS     int `yaml:"min_interval_ms"`
	MaxTTL            int `yaml:"max_ttl"`
}

func (l Limits) withDefaults() Limits {
	if l.MaxConcurrentJobs <= 0 {
		l.MaxConcurrentJobs = 20
	}
	if l.MaxCycles <= 0 {
		l.MaxCycles = 1000
	}
	if l.MinIntervalMS <= 0 {
		l.MinIntervalMS = 100
	}
	if l.MaxTTL <= 0 {
		l.MaxTTL = 40
	}
	return l
}

// Agent connects to the backend and runs the jobs it sends.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	engine *trace.Engine
	token  string
	tls    credentials.TransportCredentials

	seq  sequence
	jobs jobTable
}

// New builds an agent, opening the engine's raw sockets and reading the
// backend secrets. It does not dial: Run does, with reconnect.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	cfg.Limits = cfg.Limits.withDefaults()
	if err := cfg.Backend.Validate(); err != nil {
		return nil, err
	}
	token, err := cfg.Backend.token()
	if err != nil {
		return nil, err
	}
	tlsCfg, err := cfg.Backend.tlsConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Backend.InsecureSkipVerify {
		log.Warn("TLS certificate verification is DISABLED (insecure_skip_verify)",
			"consequence", "the backend is not authenticated; a man-in-the-middle could capture this agent's token",
			"fix", "unset insecure_skip_verify and trust the backend's CA via ca or ca_file")
	}
	eng, err := trace.New(log)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg:    cfg,
		log:    log,
		engine: eng,
		token:  token,
		tls:    credentials.NewTLS(tlsCfg),
		jobs:   jobTable{m: map[string]context.CancelFunc{}},
	}, nil
}

func (a *Agent) Close() error { return a.engine.Close() }

// Run keeps a session to the backend up, reconnecting with backoff, until
// ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		a.log.Warn("session ended, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (a *Agent) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, a.cfg.Backend.dialTimeout())
	defer cancel()

	conn, err := grpc.NewClient(a.cfg.Backend.Address,
		grpc.WithTransportCredentials(a.tls),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 20 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	md := metadata.New(map[string]string{"authorization": "Bearer " + a.token})
	stream, err := pb.NewAgentGatewayClient(conn).Session(metadata.NewOutgoingContext(ctx, md))
	if err != nil {
		return err
	}

	if err := stream.Send(&pb.AgentMessage{Msg: &pb.AgentMessage_Hello{Hello: a.hello()}}); err != nil {
		return err
	}
	welcome, err := stream.Recv()
	if err != nil {
		return err
	}
	if welcome.GetWelcome() == nil {
		return errors.New("expected Welcome")
	}
	a.log.Info("connected to backend", "site", a.cfg.Backend.Site, "address", a.cfg.Backend.Address)
	_ = dialCtx

	// One goroutine sends heartbeats; the main loop receives commands.
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()
	defer a.jobs.cancelAll()

	sendMu := &sync.Mutex{}
	send := func(m *pb.AgentMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}

	go a.heartbeat(sessCtx, send)

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch m := msg.Msg.(type) {
		case *pb.BackendMessage_Start:
			a.startJob(sessCtx, m.Start, send)
		case *pb.BackendMessage_Cancel:
			a.jobs.cancel(m.Cancel.JobId)
		}
	}
}

func (a *Agent) heartbeat(ctx context.Context, send func(*pb.AgentMessage) error) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = send(&pb.AgentMessage{Msg: &pb.AgentMessage_Heartbeat{Heartbeat: &pb.Heartbeat{
				Time: timestamppb.Now(), ActiveJobs: uint32(a.jobs.count()),
			}}})
		}
	}
}

func (a *Agent) hello() *pb.Hello {
	caps := a.engine.Capabilities()
	return &pb.Hello{
		Site:      a.cfg.Backend.Site,
		StartedAt: timestamppb.Now(),
		Capabilities: &pb.Capabilities{
			Ipv4: caps.IPv4, Ipv6: caps.IPv6, Icmp: true, Udp: true, Tcp: true,
		},
		Limits: &pb.Limits{
			MaxConcurrentJobs: uint32(a.cfg.Limits.MaxConcurrentJobs),
			MaxCycles:         uint32(a.cfg.Limits.MaxCycles),
			MinIntervalMs:     uint32(a.cfg.Limits.MinIntervalMS),
			MaxTtl:            uint32(a.cfg.Limits.MaxTTL),
		},
	}
}

// startJob resolves the target, then runs the engine in its own goroutine,
// streaming every event back tagged with the job id.
func (a *Agent) startJob(ctx context.Context, job *pb.StartJob, send func(*pb.AgentMessage) error) {
	spec := job.GetTrace()
	if spec == nil {
		a.sendError(send, job.JobId, pb.JobError_CODE_UNSPECIFIED, "no trace spec")
		return
	}
	if a.jobs.count() >= a.cfg.Limits.MaxConcurrentJobs {
		a.sendError(send, job.JobId, pb.JobError_CODE_LIMIT, "agent at job limit")
		return
	}

	resolved, err := a.resolve(ctx, spec)
	if err != nil {
		a.sendError(send, job.JobId, pb.JobError_CODE_RESOLVE, err.Error())
		return
	}

	// The source address the probes will actually leave from. When the user
	// pinned one it is that; otherwise ask the kernel which source its routing
	// would pick for this target (a UDP dial selects it without sending
	// anything). Reported so the operator sees which interface a PoP traced
	// from, which matters on a multi-homed site.
	es := specFromProto(spec, resolved)
	effSource := es.Source
	if !effSource.IsValid() {
		effSource = dialSource(resolved, es.Port)
	}

	jobCtx, cancel := context.WithCancel(ctx)
	a.jobs.add(job.JobId, cancel)

	go func() {
		defer a.jobs.remove(job.JobId)
		err := a.engine.Run(jobCtx, es, func(ev trace.Event) {
			je := eventToProto(job.JobId, a.seq.next(), ev)
			if st := je.GetStarted(); st != nil && st.Source == "" && effSource.IsValid() {
				st.Source = effSource.String()
			}
			_ = send(&pb.AgentMessage{Msg: &pb.AgentMessage_Event{Event: je}})
		})
		if err != nil {
			a.sendError(send, job.JobId, pb.JobError_CODE_ENGINE, err.Error())
		}
	}()
}

// resolve turns the target name or literal into an address, honouring the
// requested family, using the host's resolver (the site's DNS).
func (a *Agent) resolve(ctx context.Context, spec *pb.TraceSpec) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(spec.Target); err == nil {
		return addr, nil
	}
	network := "ip"
	switch spec.Family {
	case pb.AddressFamily_ADDRESS_FAMILY_IPV4:
		network = "ip4"
	case pb.AddressFamily_ADDRESS_FAMILY_IPV6:
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, network, spec.Target)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(ips) == 0 {
		return netip.Addr{}, errors.New("no addresses")
	}
	// Prefer the engine's available families; when both are asked for,
	// IPv6 first if the engine has it.
	caps := a.engine.Capabilities()
	for _, want6 := range []bool{caps.IPv6, false} {
		for _, ip := range ips {
			ip = ip.Unmap()
			if ip.Is6() == want6 && (ip.Is6() && caps.IPv6 || ip.Is4() && caps.IPv4) {
				return ip, nil
			}
		}
	}
	return ips[0].Unmap(), nil
}

// dialSource returns the source address the host's routing would use to reach
// target, by opening (not sending on) a UDP socket to it. Empty if it cannot
// be determined.
func dialSource(target netip.Addr, port uint16) netip.Addr {
	p := port
	if p == 0 {
		p = 33434
	}
	c, err := net.Dial("udp", netip.AddrPortFrom(target, p).String())
	if err != nil {
		return netip.Addr{}
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		if addr, ok := netip.AddrFromSlice(ua.IP); ok {
			return addr.Unmap()
		}
	}
	return netip.Addr{}
}

func (a *Agent) sendError(send func(*pb.AgentMessage) error, jobID string, code pb.JobError_Code, msg string) {
	_ = send(&pb.AgentMessage{Msg: &pb.AgentMessage_Event{Event: &pb.JobEvent{
		JobId: jobID, Seq: a.seq.next(), Time: timestamppb.Now(),
		Event: &pb.JobEvent_Error{Error: &pb.JobError{Code: code, Message: msg}},
	}}})
}

// sequence is the per-agent monotonic event counter.
type sequence struct {
	mu sync.Mutex
	n  uint64
}

func (s *sequence) next() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.n
}

// jobTable tracks running jobs so Cancel and disconnect can stop them.
type jobTable struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

func (j *jobTable) add(id string, cancel context.CancelFunc) {
	j.mu.Lock()
	j.m[id] = cancel
	j.mu.Unlock()
}
func (j *jobTable) remove(id string) {
	j.mu.Lock()
	delete(j.m, id)
	j.mu.Unlock()
}
func (j *jobTable) cancel(id string) {
	j.mu.Lock()
	if c := j.m[id]; c != nil {
		c()
	}
	j.mu.Unlock()
}
func (j *jobTable) cancelAll() {
	j.mu.Lock()
	for _, c := range j.m {
		c()
	}
	j.mu.Unlock()
}
func (j *jobTable) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.m)
}
