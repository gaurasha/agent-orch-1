# Architecture diagrams — one overall picture, ten focused dives

Eleven diagrams, each with a companion document that explains **services, domain
boundaries, data flow, failure handling, optimisations and trade-offs** for that
area. Every diagram is generated from a spec in [`gen/`](gen/) so it can be
regenerated, diffed and reviewed like code:

```bash
python3 docs/architecture/gen/build.py     # rewrites docs/architecture/svg/*.svg
```

The companion *reasoning* — why it is built this way, every alternative that was
considered at every decision, and the references — lives in
[`../reasoning/`](../reasoning/README.md). This folder answers **what** and
**how**; that one answers **why**.

---

## The map

| # | Diagram | Document | One-line summary |
|---|---|---|---|
| 00 | [overall](svg/00-overall.svg) | [00-overall.md](00-overall.md) | Every component, all four trust zones, every data flow — and what is deliberately absent |
| 01 | [control plane](svg/01-control-plane.svg) | [01-control-plane.md](01-control-plane.md) | Identity and intent enter here; it holds no credentials and executes nothing |
| 02 | [scheduling & runtime](svg/02-scheduling-runtime.svg) | [02-scheduling-runtime.md](02-scheduling-runtime.md) | A run is a row plus a log; workers lease, replay, step, fenced-commit |
| 03 | [tool gateway](svg/03-tool-gateway.svg) | [03-tool-gateway.md](03-tool-gateway.md) | The single choke point, stage by stage, with every refusal |
| 04 | [sandbox](svg/04-sandbox.svg) | [04-sandbox.md](04-sandbox.md) | The agent jail from raw Linux primitives, layer by layer, three drivers |
| 05 | [credential plane](svg/05-credential-plane.svg) | [05-credential-plane.md](05-credential-plane.md) | How an agent uses a secret it can never read |
| 06 | [model plane & fairness](svg/06-model-plane.svg) | [06-model-plane.md](06-model-plane.md) | One choke point for the most expensive resource; weighted max-min fairness |
| 07 | [state & failover](svg/07-state-failover.svg) | [07-state-failover.md](07-state-failover.md) | Six tables, four transactions, every failure converges on the same row |
| 08 | [Kubernetes topology](svg/08-network-topology.svg) | [08-network-topology.md](08-network-topology.md) | Namespaces, NetworkPolicy, RBAC, RuntimeClass, GitOps |
| 09 | [failure map](svg/09-failure-map.svg) | [09-failure-map.md](09-failure-map.md) | Every failure → the mechanism that absorbs it → what is not handled |
| 10 | [observability](svg/10-observability.svg) | [10-observability.md](10-observability.md) | Four signals, one source of truth |

---

## How to read the diagrams

**Colours are trust levels, not components.** The same palette is used in all
eleven pictures:

| Colour | Meaning | Examples |
|---|---|---|
| grey | people and browsers | operator console, CI caller |
| red | outside our control | LLM provider, GitHub, the documents agents read |
| blue | trusted platform code | control plane, workers, model gateway, limiter |
| purple | the **trusted computing base** — the only code that ever holds a plaintext credential | tool gateway, credential broker, egress proxy |
| green | durable state | Postgres, object storage |
| orange | untrusted execution | the agent sandbox running model-authored code |
| amber | a trusted binary holding a credential, fed hostile input | the broker sandbox running `gh` |

**Dashed zones are enforcement boundaries**, not documentation boundaries. Each
one corresponds to something a machine enforces: a Kubernetes namespace with a
NetworkPolicy, a Linux namespace set, or a process that is the only holder of a
key. If a line crosses a zone in the picture, a packet or a syscall crosses a
real boundary in the system.

**Arrows are data flows that exist.** Equally important is what is *missing*:
the overall diagram ends with a box listing the three arrows that are
deliberately absent. Most of the security argument is that those arrows cannot
be drawn.

**"(designed)"** on a box means the component is in the design and the
manifests reference it, but the proof-of-concept does not build it. Every such
gap is listed in [README.md — what is faked](../../README.md) and in the
"Designed, not built" tables inside diagrams 07 and 09.

---

## How the diagrams are built

Hand-drawn diagrams rot: one edit moves everything, and nobody re-checks the
picture against the code. These are generated instead:

* [`gen/svgkit.py`](gen/svgkit.py) — a ~400-line declarative SVG generator:
  zones, boxes, orthogonal arrows with waypoints, notes, tables, legends, a fixed
  palette. No dependencies beyond the Python standard library.
* [`gen/dNN_*.py`](gen/) — one spec per diagram. Coordinates are explicit
  because dense architecture diagrams are exactly the case where auto-layout
  (Graphviz, Mermaid, ELK) produces crossings through boxes; every route here
  was placed and then **checked by rendering the SVG in headless Chromium and
  looking at it**.
* [`gen/build.py`](gen/build.py) — regenerates every SVG.

The SVGs are self-contained (own background, system fonts, no external
references) so they render identically on GitHub, in the interactive artifact
and when printed.

---

## Reading paths

* **"Show me the whole thing"** → [00-overall](00-overall.md), then
  [09-failure-map](09-failure-map.md).
* **"I am reviewing the security model"** → [03-tool-gateway](03-tool-gateway.md),
  [04-sandbox](04-sandbox.md), [05-credential-plane](05-credential-plane.md),
  [08-network-topology](08-network-topology.md).
* **"I am reviewing the durability story"** → [02-scheduling-runtime](02-scheduling-runtime.md),
  [07-state-failover](07-state-failover.md).
* **"I am going to operate it"** → [08-network-topology](08-network-topology.md),
  [10-observability](10-observability.md), [09-failure-map](09-failure-map.md).
* **"I want the rationale, not the picture"** → [`../reasoning/`](../reasoning/README.md).
