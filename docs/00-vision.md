# 00 — Vision and Strategic Positioning

## The thesis in one paragraph

Every AI agent in production today calls tools through the Model Context Protocol or something similar. None of those calls are inspected, audited, or constrained. Within twelve months a high-profile incident — exfiltrated secrets, destroyed filesystems, leaked customer data — will make this a board-level concern. The proxy that sits between AI clients and MCP servers will become as standard as the reverse proxy that sits between the internet and a web service. We are building that proxy.

## What we are not

We are not a prompt firewall vendor. We are not an LLM guardrails company. We are not a "responsible AI" platform. Those framings are already crowded (Lakera, Lasso, Prompt Security, Protect AI, Cloudflare AI Gateway, Invariant Labs) and they all skate above the protocol layer. We sit *at* the protocol layer, on the user's machine, in the path of every tool call. That is a position none of those incumbents own.

We are also not a marketplace, an MCP server vendor, or a model trainer. We do not compete with Anthropic, with individual MCP server authors, or with foundation model providers. We are infrastructure that makes their ecosystem safe to use.

## The wedge

A single Go binary that runs locally, proxies any MCP server, logs everything, and blocks the obvious attacks. Free, open source, drop-in. No account, no cloud, no signup. The user installs it because their AI agent did something they did not expect and they want to know what happened. After that, they leave it running.

This is the wedge for three reasons:

1. **Distribution is free.** Developers install open-source tools without procurement. Enterprise security tools die in procurement.
2. **The need is immediate.** Today, right now, no MCP user has any idea what their agents are doing. We solve that on day one.
3. **The position compounds.** Once the proxy is in the path, adding policy, alerting, team features, and ML detection becomes additive instead of greenfield.

## The moat

The moat is not the proxy. The proxy is commoditizable — Cloudflare or any infrastructure company could ship a competing one in a quarter. The moat is the **dataset of real MCP tool-call traffic** that the proxy collects (opt-in) from thousands of installs.

- Lakera, PromptGuard, Llama Guard are trained on synthetic and public attack corpora.
- Nobody has real MCP tool-call data because the protocol is too new and nobody is in the path.
- We will be the first and only entity with that data.
- That dataset becomes the training set for behavioral anomaly detection, which is the actual paid product.

This is why we ship the dumb regex-only version first and resist the urge to bolt on third-party ML detectors. Bolting on someone else's model means we never collect the data that makes our own model defensible.

## The product staircase

| Version | Time | Audience | Pricing | What ships |
|---------|------|----------|---------|------------|
| v0.1 | Now → 6 weeks | Individual developers | Free | Local proxy, audit log, pattern detection, dashboard |
| v0.2 | Month 3-6 | Small teams, design partners | Free | Cloud sync, team policies, Slack alerts, opt-in telemetry collection |
| v0.3 | Month 6-9 | Teams that have seen value | $20/dev/mo | Behavioral anomaly detection trained on our dataset |
| v1.0 | Month 12+ | Enterprises | $50k+/yr | SSO, RBAC, audit export, on-prem, sandboxed execution, custom policies |

Each tier funds the next. We do not chase enterprise revenue before we have the dataset to justify the product.

## What we will not do, and why

**No third-party ML integrations in v0.1 (Lakera, PromptGuard, Llama Guard, etc.)**
Integrating these makes us dependent on someone else's model and corrupts the IP story. We have to rip them out later anyway. Better to ship regex-only and look "dumb" for six months than ship something we cannot own end-to-end.

**No sandboxing in v0.1.**
Local sandboxing on macOS/Windows is either theater (same-machine isolation) or platform-specific pain (entitlements, AppContainer). Real sandboxing is a cloud-execution feature for v1.0. v0.1 uses process-level guardrails (env var stripping, path allowlists, network egress lists) which deliver 80% of the security value for 5% of the implementation cost.

**No cloud features in v0.1.**
Cloud features require accounts, accounts require signup, signup kills install rates. v0.1 is fully local. v0.2 introduces an optional cloud backend that existing users can opt into.

**No marketplace, no MCP server hosting, no "verified servers" registry.**
Anthropic and the MCP working group will own this. Competing is a losing battle and a distraction.

**No focus on the model layer.**
We do not inspect LLM prompts. We inspect tool calls. Prompt firewalls are dying because the attack surface is moving from prompts to tools, and we are positioned at the tool boundary.

## Competitive landscape

| Vendor | What they do | Why we are different |
|--------|--------------|----------------------|
| Lakera Guard | Cloud API for prompt injection detection | Cloud-only, prompt-layer, not MCP-aware |
| Prompt Security | Enterprise prompt firewall | Enterprise sales motion, prompt-layer |
| Protect AI | ML model scanning + LLM firewall | Focus is model supply chain, not runtime tool calls |
| Invariant Labs | Agent security policies | DSL-based, no proxy, no telemetry, research-led |
| Cloudflare AI Gateway | Generic AI traffic proxy | HTTP-layer, not MCP-aware, no policy depth |
| Anthropic | MCP spec authors | Will not build runtime security themselves; they want an ecosystem |

The honest threat is Cloudflare deciding to ship an MCP-aware product. Our defense is a 12-18 month head start, a focused product, and a dataset they cannot collect retroactively.

## Naming

Working name: **Sentinel**. Boring, descriptive, greppable, available as a domain at time of writing. If the strategic positioning matures into "Zero Trust for AI Agents," the name can become **Sentinel**, **Aperture**, or **Outpost** — all evocative, all available in some form. We do not rename until we have product-market fit.

## Success criteria

| Phase | Success looks like |
|-------|--------------------|
| v0.1 | 100 GitHub stars, 10 active design partners, 1 public incident where Sentinel flagged something real |
| v0.2 | 1,000 active installs, opt-in telemetry from 100+, first paid pilot |
| v0.3 | 10,000 active installs, $50k ARR, behavioral detector outperforms regex baseline on internal benchmarks |
| v1.0 | $1M ARR, three enterprise logos, recognized as the category-defining product |

If v0.1 does not hit 100 stars and 10 design partners in 8 weeks, we have a positioning or distribution problem and should pause to rethink rather than ship more features.

## Risks

1. **Anthropic ships native security in Claude Desktop.** Mitigation: be deeply integrated by then, support every MCP client not just Claude.
2. **Cloudflare ships an MCP-aware AI Gateway.** Mitigation: own the local-machine position they will not enter; own the dataset.
3. **MCP loses to a competing standard.** Mitigation: the proxy architecture generalizes — we can support OpenAI's tool protocol, A2A, or whatever wins.
4. **Nobody cares until a breach happens.** Mitigation: lead with *visibility* ("see what your agents did") not security. Visibility is a felt need today.
5. **Telemetry opt-in rate is too low to build a dataset.** Mitigation: make telemetry obviously valuable to the user (better detection, anonymized community attack feed), default-off but prominent.

## Related docs

- [[01-architecture]] — how the system is structured
- [[02-mcp-protocol]] — why this works for every MCP server
- [[13-v01-roadmap]] — concrete 6-week plan
- [[18-privacy-model]] — the privacy commitments that make telemetry trustworthy
