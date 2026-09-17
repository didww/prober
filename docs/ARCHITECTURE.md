# prober — architecture

A rewrite of mtr-web. The old app ran one Flask + AngularJS instance per PoP,
each shelling out to `mtr` and exposing a WebSocket the browser connected to
directly. This replaces it with three parts and a native probing engine.

> **Name.** "prober" is a placeholder for the product and the component
> prefix. It will be renamed later (candidates included Argus, Ariadne,
> Meridian). Keep the name in as few places as possible: the module path, the
> `PROBER_` env prefix, the metrics namespace, the proto package and the image
> names.

## Components

- **prober-agent** — one per PoP. Holds the native traceroute engine
  (`internal/trace`). Dials the backend and keeps one long-lived gRPC stream;
  never listens for inbound connections. Needs `CAP_NET_RAW`.
- **prober-backend** — central. Three listeners: gRPC for agents (server TLS +
  per-agent bearer token), HTTP for the browser (SPA + REST + SSE, OIDC
  login), and Prometheus metrics. The only client of the agents; the only
  ingress for the browser.
- **web/** — the TypeScript SPA (Vue 3), built into the backend binary. A
  ping.pe-style page: one row per site, live per-cycle cells, expandable to a
  full per-site hop table.

## The engine (`internal/trace`)

Implements traceroute and ping directly, with no `mtr` process:

- One shared raw ICMP socket per address family per process; a dispatcher
  routes each reply to the run that sent the probe. Sends set the TTL per
  packet (socket option on IPv4, control message on IPv6).
- Protocols: ICMP echo, UDP (port encodes nothing; the socket's local port is
  the probe identity), and connect-based TCP (half-open, `TCP_SYNCNT=1`).
- Per hop: sent, received, loss, last, best, avg (Welford), worst, stdev,
  jitter, and every address seen (ECMP). Reverse DNS is async and never
  blocks a cycle.
- One `CycleDone` event per cycle carries every hop's sample plus aggregates,
  so a run streams one event per interval, not one per hop.
- Options: protocol, family, port, cycles, interval, first/max TTL, packet
  size, DSCP, source address, ping mode, probe timeout.

### Testing

`internal/trace` unit tests run anywhere. The integration tests need raw
sockets and run as root inside an unprivileged user+network namespace
(`make test-trace`): loopback for every protocol/family, and a veth chain of
network namespaces (test ── r1 ── r2 ── target) that produces real
time-exceeded replies for genuine multi-hop coverage.

## Roadmap

1. **Now.** Real-time traces started from the browser.
2. **Later.** Monitoring of predefined targets: the backend pushes a target
   list; each agent runs its own schedule and reports; the backend stores
   results in VictoriaLogs. The stream's job/event messages are already
   `oneof`s, and events carry an agent timestamp and sequence, so this is
   additive.
3. **Later.** The same, with SIP OPTIONS ping as a second probe kind.

## Status

- [x] Repo skeleton, module, proto contract (`api/proto/prober/v1`), codegen.
- [x] Engine: ICMP/UDP/TCP, v4/v6, stats, cancel, tests (unit + namespace).
- [ ] prober-agent: gRPC client, job manager, capability probe, limits.
- [ ] prober-backend: gateway, run manager, REST + SSE, three listeners.
- [ ] Auth (OIDC, from vlui), policy, rate limits.
- [ ] Frontend (Vue 3), ping.pe-style UI.
- [ ] Packaging: deb + Helm chart per component, distroless images, GH Actions.
