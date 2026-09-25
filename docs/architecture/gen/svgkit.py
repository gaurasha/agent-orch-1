"""
A small declarative SVG generator for architecture diagrams.

Why this exists rather than hand-written SVG or Mermaid:
  - Hand-written SVG means pixel-hunting coordinates; one edit shifts everything.
  - Mermaid cannot draw nested trust zones with annotations, side-notes, or
    precise anchor placement, and its auto-layout fights dense diagrams.
  - A tiny spec → SVG step gives computed anchors, consistent styling, and a
    reproducible build:  python3 gen/build.py  regenerates every diagram.

Everything is self-contained SVG with its own background so it renders
identically on GitHub light/dark, in the artifact, and when printed.
"""
from __future__ import annotations
import html
from dataclasses import dataclass, field

# ── palette ─────────────────────────────────────────────────────────────────
BG        = "#f7f8fa"
INK       = "#1f2328"
MUTED     = "#57606a"
LINE      = "#8c959f"
WHITE     = "#ffffff"

# component kinds → (fill, accent)
KIND = {
    "platform":  ("#eaf2fb", "#2f6fbf"),   # blue   – trusted platform code
    "tcb":       ("#efe7fb", "#7a3fd1"),   # purple – the trusted computing base
    "state":     ("#e7f6ec", "#2e8b57"),   # green  – durable state
    "sandbox":   ("#fdf1e3", "#d9822b"),   # orange – untrusted execution
    "broker":    ("#fff7d6", "#b8860b"),   # amber  – trusted binary w/ credential
    "external":  ("#fbe9e9", "#c0392b"),   # red    – outside our control
    "human":     ("#f0f0f0", "#6e7781"),   # grey   – people
    "neutral":   ("#f3f4f6", "#6e7781"),
    "k8s":       ("#eaf0fc", "#326ce5"),   # blue   – Kubernetes objects
}
ZONE = {
    "untrusted": ("#c0392b", "rgba(192,57,43,0.045)"),
    "platform":  ("#2f6fbf", "rgba(47,111,191,0.045)"),
    "tcb":       ("#7a3fd1", "rgba(122,63,209,0.06)"),
    "state":     ("#2e8b57", "rgba(46,139,87,0.05)"),
    "sandbox":   ("#d9822b", "rgba(217,130,43,0.05)"),
    "k8s":       ("#326ce5", "rgba(50,108,229,0.04)"),
    "neutral":   ("#6e7781", "rgba(110,119,129,0.05)"),
    "broker":    ("#b8860b", "rgba(184,134,11,0.06)"),
    "external":  ("#c0392b", "rgba(192,57,43,0.045)"),
    "human":     ("#6e7781", "rgba(110,119,129,0.05)"),
}

FONT = "ui-sans-serif, -apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"
MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"


def esc(s: str) -> str:
    return html.escape(s, quote=True)


@dataclass
class Box:
    id: str
    x: float; y: float; w: float; h: float
    title: str
    lines: list[str] = field(default_factory=list)
    kind: str = "platform"
    mono: bool = False
    badge: str | None = None       # small tag top-right, e.g. "×2" or "HPA 2–40"

    @property
    def cx(self): return self.x + self.w / 2
    @property
    def cy(self): return self.y + self.h / 2

    def anchor(self, side: str):
        return {
            "n": (self.cx, self.y), "s": (self.cx, self.y + self.h),
            "w": (self.x, self.cy), "e": (self.x + self.w, self.cy),
        }[side]


@dataclass
class Zone:
    x: float; y: float; w: float; h: float
    title: str
    kind: str = "neutral"
    subtitle: str | None = None
    subtitle_inside: bool = False   # draw the subtitle under the tab, inside the zone
    subtitle_align: str = "left"    # "left" (after the tab) | "right" (end of the top edge)
    subtitle_x: float | None = None # explicit x for the subtitle on the top edge


@dataclass
class Arrow:
    src: str; dst: str
    label: str | None = None
    style: str = "solid"          # solid | dashed | dotted
    color: str = LINE
    src_side: str | None = None   # force anchor side
    dst_side: str | None = None
    label_dx: float = 0
    label_dy: float = 0
    bidir: bool = False
    via: list[tuple[float, float]] | None = None   # optional waypoints
    width: float = 1.6
    src_off: float = 0            # shift the source anchor along its edge
    dst_off: float = 0            # shift the destination anchor along its edge
    label_at: tuple[float, float] | None = None   # absolute label position (overrides midpoint)


@dataclass
class Note:
    x: float; y: float; w: float
    lines: list[str]
    title: str | None = None
    kind: str = "neutral"


@dataclass
class Text:
    x: float; y: float
    lines: list[str]
    size: float = 11
    color: str = INK
    mono: bool = False
    weight: str = "normal"
    anchor: str = "start"


@dataclass
class Line:
    x1: float; y1: float; x2: float; y2: float
    color: str = LINE
    style: str = "solid"
    width: float = 1.4
    marker: bool = False
    via: list[tuple[float, float]] | None = None


@dataclass
class Table:
    x: float; y: float
    cols: list[float]                 # column widths
    rows: list[list[list[str]]]       # rows → cells → lines
    header: bool = True
    row_h: list[float] | None = None  # explicit row heights, else computed
    kind: str = "neutral"
    cell_kind: list[list[str | None]] | None = None   # optional per-cell colour kind
    size: float = 10.5


class Diagram:
    def __init__(self, width: int, height: int, title: str, subtitle: str = ""):
        self.w, self.h = width, height
        self.title, self.subtitle = title, subtitle
        self.zones: list[Zone] = []
        self.boxes: dict[str, Box] = {}
        self.arrows: list[Arrow] = []
        self.notes: list[Note] = []
        self.texts: list[Text] = []
        self.lines: list[Line] = []
        self.tables: list[Table] = []
        self.legend: list[tuple[str, str]] = []   # (kind, label)

    # ── builders ────────────────────────────────────────────────────────
    def zone(self, *a, **k):  self.zones.append(Zone(*a, **k)); return self
    def box(self, *a, **k):   b = Box(*a, **k); self.boxes[b.id] = b; return self
    def arrow(self, *a, **k): self.arrows.append(Arrow(*a, **k)); return self
    def note(self, *a, **k):  self.notes.append(Note(*a, **k)); return self
    def text(self, *a, **k):  self.texts.append(Text(*a, **k)); return self
    def line(self, *a, **k):  self.lines.append(Line(*a, **k)); return self
    def table(self, *a, **k): self.tables.append(Table(*a, **k)); return self

    # ── geometry ────────────────────────────────────────────────────────
    def _pick_sides(self, a: Box, b: Box):
        dx, dy = b.cx - a.cx, b.cy - a.cy
        if abs(dx) * a.h > abs(dy) * a.w * 1.15:      # mostly horizontal
            return ("e", "w") if dx > 0 else ("w", "e")
        return ("s", "n") if dy > 0 else ("n", "s")

    # ── render ──────────────────────────────────────────────────────────
    def _text_block(self, x, y, lines, size=11.5, color=INK, mono=False, weight="normal", anchor="start", lh=None):
        lh = lh or size * 1.42
        fam = MONO if mono else FONT
        out = []
        for i, ln in enumerate(lines):
            out.append(f'<text x="{x:.1f}" y="{y + i*lh:.1f}" font-family="{fam}" font-size="{size}" '
                       f'font-weight="{weight}" fill="{color}" text-anchor="{anchor}">{esc(ln)}</text>')
        return "\n".join(out)

    def render(self) -> str:
        o = []
        o.append(f'<svg xmlns="http://www.w3.org/2000/svg" width="{self.w}" height="{self.h}" '
                 f'viewBox="0 0 {self.w} {self.h}" font-family="{FONT}">')
        o.append(f'''<defs>
  <marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="{LINE}"/></marker>
  <marker id="arr-ink" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="{INK}"/></marker>
  <marker id="arr-red" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="#c0392b"/></marker>
  <marker id="arr-purple" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="#7a3fd1"/></marker>
  <marker id="arr-green" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="#2e8b57"/></marker>
  <marker id="arr-orange" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="#d9822b"/></marker>
  <filter id="shadow" x="-5%" y="-5%" width="110%" height="115%">
    <feDropShadow dx="0" dy="1" stdDeviation="1.2" flood-color="#000" flood-opacity="0.10"/></filter>
</defs>''')
        o.append(f'<rect width="{self.w}" height="{self.h}" fill="{BG}"/>')

        # title
        o.append(self._text_block(28, 36, [self.title], size=20, weight="700"))
        if self.subtitle:
            o.append(self._text_block(28, 56, [self.subtitle], size=12, color=MUTED))

        # zones (back to front as declared)
        for z in self.zones:
            stroke, fill = ZONE[z.kind]
            o.append(f'<rect x="{z.x}" y="{z.y}" width="{z.w}" height="{z.h}" rx="10" ry="10" '
                     f'fill="{fill}" stroke="{stroke}" stroke-width="1.4" stroke-dasharray="7 5"/>')
            # title tab
            tw = 9.6 * len(z.title) + 26
            o.append(f'<rect x="{z.x + 12}" y="{z.y - 11}" width="{tw:.0f}" height="22" rx="6" fill="{stroke}"/>')
            o.append(f'<text x="{z.x + 12 + tw/2:.1f}" y="{z.y + 4.5}" font-size="11.5" font-weight="700" '
                     f'fill="{WHITE}" text-anchor="middle" letter-spacing="0.4">{esc(z.title)}</text>')
            if z.subtitle:
                sw = 5.6 * len(z.subtitle) + 10
                if z.subtitle_inside:
                    o.append(f'<text x="{z.x + 12:.1f}" y="{z.y + 26}" font-size="11" fill="{stroke}" '
                             f'font-style="italic">{esc(z.subtitle)}</text>')
                else:
                    if z.subtitle_x is not None:
                        sx, anchor = z.subtitle_x, "start"
                        bx = sx - 5
                    elif z.subtitle_align == "right":
                        sx, anchor = z.x + z.w - 50, "end"
                        bx = sx - sw + 5
                    else:
                        sx, anchor = z.x + 12 + tw + 10, "start"
                        bx = z.x + 12 + tw
                    o.append(f'<rect x="{bx:.1f}" y="{z.y - 7}" width="{sw:.0f}" height="14" fill="{BG}"/>')
                    o.append(f'<text x="{sx:.1f}" y="{z.y + 4.5}" font-size="11" fill="{stroke}" '
                             f'font-style="italic" text-anchor="{anchor}">{esc(z.subtitle)}</text>')

        # free lines (under boxes)
        for ln in self.lines:
            pts = [(ln.x1, ln.y1)] + (ln.via or []) + [(ln.x2, ln.y2)]
            path = "M " + " L ".join(f"{x:.1f} {y:.1f}" for x, y in pts)
            dash = {"solid": "", "dashed": 'stroke-dasharray="7 5"', "dotted": 'stroke-dasharray="2 4"'}[ln.style]
            marker = {LINE: "arr", INK: "arr-ink", "#c0392b": "arr-red", "#7a3fd1": "arr-purple",
                      "#2e8b57": "arr-green", "#d9822b": "arr-orange"}.get(ln.color, "arr")
            me = f'marker-end="url(#{marker})"' if ln.marker else ""
            o.append(f'<path d="{path}" fill="none" stroke="{ln.color}" stroke-width="{ln.width}" {dash} {me}/>')

        # arrows (under boxes so boxes occlude line ends cleanly); labels are
        # deferred to a pass after the boxes so a label is never hidden
        labels = []
        for a in self.arrows:
            s, d = self.boxes[a.src], self.boxes[a.dst]
            ss, ds = self._pick_sides(s, d)
            ss = a.src_side or ss
            ds = a.dst_side or ds
            (x1, y1), (x2, y2) = s.anchor(ss), d.anchor(ds)
            # an offset along the edge: horizontal edges shift x, vertical edges shift y
            if ss in ("n", "s"): x1 += a.src_off
            else:                y1 += a.src_off
            if ds in ("n", "s"): x2 += a.dst_off
            else:                y2 += a.dst_off
            pts = [(x1, y1)] + (a.via or []) + [(x2, y2)]
            path = "M " + " L ".join(f"{x:.1f} {y:.1f}" for x, y in pts)
            dash = {"solid": "", "dashed": 'stroke-dasharray="7 5"', "dotted": 'stroke-dasharray="2 4"'}[a.style]
            marker = {LINE: "arr", INK: "arr-ink", "#c0392b": "arr-red", "#7a3fd1": "arr-purple",
                      "#2e8b57": "arr-green", "#d9822b": "arr-orange"}.get(a.color, "arr")
            ms = f'marker-start="url(#{marker})"' if a.bidir else ""
            o.append(f'<path d="{path}" fill="none" stroke="{a.color}" stroke-width="{a.width}" {dash} '
                     f'marker-end="url(#{marker})" {ms}/>')
            if a.label:
                # label at the midpoint of the (possibly multi-segment) path
                mx, my = a.label_at if a.label_at else self._mid(pts)
                mx += a.label_dx; my += a.label_dy
                lines = a.label.split("\n")
                lw = max(len(l) for l in lines) * 6.0 + 12
                lhgt = 14 * len(lines) + 6
                labels.append(f'<rect x="{mx - lw/2:.1f}" y="{my - lhgt/2:.1f}" width="{lw:.1f}" height="{lhgt}" '
                              f'rx="4" fill="{BG}" fill-opacity="0.94"/>')
                for i, ln in enumerate(lines):
                    labels.append(f'<text x="{mx:.1f}" y="{my - lhgt/2 + 12 + i*14:.1f}" font-size="10.5" '
                                  f'fill="{a.color if a.color != LINE else MUTED}" text-anchor="middle" '
                                  f'font-family="{MONO if ln.startswith("`") else FONT}">{esc(ln.strip("`"))}</text>')

        # boxes
        for b in self.boxes.values():
            fill, accent = KIND[b.kind]
            o.append(f'<g filter="url(#shadow)">')
            o.append(f'<rect x="{b.x}" y="{b.y}" width="{b.w}" height="{b.h}" rx="7" fill="{fill}" '
                     f'stroke="{accent}" stroke-width="1.2"/>')
            o.append(f'<rect x="{b.x}" y="{b.y}" width="5" height="{b.h}" rx="2.5" fill="{accent}"/>')
            o.append('</g>')
            o.append(self._text_block(b.x + 13, b.y + 19, [b.title], size=12.5, weight="700"))
            if b.badge:
                bw = 6.6 * len(b.badge) + 12
                o.append(f'<rect x="{b.x + b.w - bw - 8:.1f}" y="{b.y + 7}" width="{bw:.1f}" height="16" rx="8" '
                         f'fill="{accent}" fill-opacity="0.15" stroke="{accent}" stroke-width="0.8"/>')
                o.append(f'<text x="{b.x + b.w - 8 - bw/2:.1f}" y="{b.y + 18.5}" font-size="9.5" '
                         f'font-weight="600" fill="{accent}" text-anchor="middle" font-family="{MONO}">{esc(b.badge)}</text>')
            if b.lines:
                o.append(self._text_block(b.x + 13, b.y + 37, b.lines, size=10.5, color=MUTED,
                                          mono=b.mono, lh=14.5))

        # arrow labels, on top of boxes
        o.extend(labels)

        # notes
        for n in self.notes:
            stroke, fill = ZONE[n.kind]
            nh = 14 * len(n.lines) + (24 if n.title else 12) + 6
            o.append(f'<rect x="{n.x}" y="{n.y}" width="{n.w}" height="{nh}" rx="6" fill="{WHITE}" '
                     f'stroke="{stroke}" stroke-width="1" stroke-dasharray="3 3"/>')
            y = n.y + 16
            if n.title:
                o.append(self._text_block(n.x + 10, y, [n.title], size=11, weight="700", color=stroke))
                y += 17
            o.append(self._text_block(n.x + 10, y, n.lines, size=10.5, color=INK, lh=14))

        # tables
        for t in self.tables:
            stroke, tint = ZONE[t.kind]
            lh = t.size * 1.36
            # wrap every cell to its column width (≈0.53 em per character for this sans face)
            def wrap(cell, cw):
                maxc = max(8, int((cw - 14) / (t.size * 0.53)))
                out = []
                for ln in cell:
                    words, cur = ln.split(" "), ""
                    for wd in words:
                        if cur and len(cur) + 1 + len(wd) > maxc:
                            out.append(cur); cur = wd
                        else:
                            cur = (cur + " " + wd) if cur else wd
                    out.append(cur)
                return out
            t.rows = [[wrap(c, t.cols[ci]) for ci, c in enumerate(row)] for row in t.rows]
            heights = t.row_h or [max(len(c) for c in row) * lh + 12 for row in t.rows]
            y = t.y
            for ri, row in enumerate(t.rows):
                x = t.x
                rh = heights[ri]
                for ci, cell in enumerate(row):
                    cw = t.cols[ci]
                    is_head = t.header and ri == 0
                    ck = (t.cell_kind[ri][ci] if t.cell_kind and t.cell_kind[ri][ci] else None)
                    if is_head:
                        fill, fg, weight = stroke, WHITE, "700"
                    elif ck:
                        fill, fg, weight = KIND[ck][0], INK, "normal"
                    else:
                        fill, fg, weight = (WHITE if ri % 2 else "#fbfbfc"), INK, "normal"
                    o.append(f'<rect x="{x}" y="{y}" width="{cw}" height="{rh:.1f}" fill="{fill}" '
                             f'stroke="{stroke}" stroke-opacity="0.45" stroke-width="0.8"/>')
                    if ci == 0 and not is_head:
                        weight = "600"
                    o.append(self._text_block(x + 7, y + 6 + t.size, cell, size=t.size, color=fg,
                                              weight=weight, lh=lh))
                    x += cw
                y += rh

        # free text
        for tx in self.texts:
            o.append(self._text_block(tx.x, tx.y, tx.lines, size=tx.size, color=tx.color,
                                      mono=tx.mono, weight=tx.weight, anchor=tx.anchor))

        # legend
        if self.legend:
            lx, ly = 28, self.h - 22 - 18 * ((len(self.legend) + 3) // 4)
            for i, (kind, label) in enumerate(self.legend):
                col, row = i % 4, i // 4
                x = lx + col * 230; y = ly + row * 18
                fill, accent = KIND[kind]
                o.append(f'<rect x="{x}" y="{y - 10}" width="14" height="12" rx="3" fill="{fill}" stroke="{accent}" stroke-width="1"/>')
                o.append(self._text_block(x + 20, y, [label], size=10.5, color=MUTED))

        o.append('</svg>')
        return "\n".join(o)

    @staticmethod
    def _mid(pts):
        # midpoint along the polyline by length
        import math
        segs = [(pts[i], pts[i+1]) for i in range(len(pts)-1)]
        total = sum(math.dist(a, b) for a, b in segs)
        acc = 0
        for a, b in segs:
            L = math.dist(a, b)
            if acc + L >= total / 2:
                t = (total/2 - acc) / L if L else 0
                return a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t
            acc += L
        return pts[-1]

    def save(self, path: str):
        with open(path, "w", encoding="utf-8") as f:
            f.write(self.render())
        return path
