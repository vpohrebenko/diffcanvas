// Bootstrap: panes, canvas transform, selection wiring, layout persistence.

import { api } from './api.js';
import { state, el, toast, debounce, DRAG_TYPE, BASE_ROW_H, BASE_FONT, rowH } from './store.js';
import { renderTree } from './tree.js';
import {
  createCard, removeCard, setCardListener, applyLOD, rebuildAll, setCollapsed,
  redrawStrips, relayoutAll, jumpChange, applyFontScale,
} from './card.js';
import {
  initArrows, renderArrows, loadArrows, toWorld, deleteSelectedArrow, linkImports,
  addArrow,
} from './arrows.js';
import {
  select, clearSelection, selectAll, selectedCards, startMarquee,
  setSelectionListener, refreshSelection,
} from './selection.js';

/**
 * Publishes the toolbar's real height into --toolbar-h.
 *
 * The sidebar and canvas are positioned from that variable, so if the toolbar
 * grows — wrapped controls, larger text, a zoom or scaling setting — they move
 * down with it instead of being overlapped, and nothing in the bar is ever
 * clipped or pushed off screen.
 */
function trackToolbarHeight() {
  const bar = document.getElementById('toolbar');
  const apply = () => {
    const h = Math.ceil(bar.getBoundingClientRect().height);
    if (h > 0) document.documentElement.style.setProperty('--toolbar-h', `${h}px`);
  };
  apply();
  if (window.ResizeObserver) new ResizeObserver(apply).observe(bar);
  window.addEventListener('resize', apply);
}

const canvas = document.getElementById('canvas');
const world = document.getElementById('world');
const minimap = document.getElementById('minimap');
const zoomBadge = document.getElementById('zoom-badge');
const emptyState = document.getElementById('empty-state');

let allPaths = [];

// ── canvas transform ─────────────────────────────────────────────────────

function applyTransform() {
  // Whole-pixel translation: a fractional offset makes every glyph land
  // between device pixels, which is most of why text looked soft at 100%.
  const px = Math.round(state.pan.x);
  const py = Math.round(state.pan.y);
  world.style.transform = `translate(${px}px, ${py}px) scale(${state.scale})`;
  zoomBadge.textContent = `${Math.round(state.scale * 100)}%`;
  applyLOD();
  drawMinimap();
}

/**
 * Something moved: cheap, and safe to run on every pointer frame.
 */
function onGeometryChange() {
  renderArrows();
  drawMinimap();
  saveLayoutSoon();
}

/**
 * The set of cards changed: also refreshes the sidebar and the empty state.
 *
 * Kept apart because refreshTreeMarks walks every row in the sidebar — 1,600
 * of them on a large review — and was running on every pointermove of a drag
 * to update something that only changes when a card opens or closes.
 */
function onCardsChanged() {
  onGeometryChange();
  emptyState.style.display = state.cards.length ? 'none' : 'grid';
  refreshTreeMarks();
  syncSelectionBar();
}

function refreshTreeMarks() {
  const open = new Set(state.cards.map(c => c.path));
  for (const row of document.querySelectorAll('.node.file')) {
    row.classList.toggle('open-on-canvas', open.has(row.dataset.path));
  }
}

function syncSelectionBar() {
  const bar = document.getElementById('selection-bar');
  const n = state.selection.size;
  bar.hidden = n === 0;
  if (n) document.getElementById('sel-count').textContent = `${n} selected`;
}

/**
 * Background drag: pan by default, marquee with shift.
 *
 * preventDefault is essential — without it the browser starts a text selection
 * underneath the drag and paints the canvas blue.
 */
canvas.addEventListener('pointerdown', ev => {
  const onEmpty = ev.target === canvas || ev.target === world || ev.target.id === 'cards';
  if (!onEmpty) return;
  ev.preventDefault();

  state.selectedArrow = null;
  renderArrows();

  if (ev.shiftKey) {
    startMarquee(canvas, ev, toWorld, ev.ctrlKey || ev.metaKey);
    return;
  }
  clearSelection();

  const startX = ev.clientX - state.pan.x;
  const startY = ev.clientY - state.pan.y;
  canvas.classList.add('panning');
  document.body.classList.add('dragging');

  const move = e => {
    state.pan.x = e.clientX - startX;
    state.pan.y = e.clientY - startY;
    applyTransform();
  };
  const up = () => {
    canvas.classList.remove('panning');
    document.body.classList.remove('dragging');
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    saveLayoutSoon();
  };
  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
});

canvas.addEventListener('wheel', ev => {
  if (ev.target.closest('.card-body')) return; // let cards scroll
  ev.preventDefault();

  const rect = canvas.getBoundingClientRect();
  const mx = ev.clientX - rect.left;
  const my = ev.clientY - rect.top;
  const factor = ev.deltaY < 0 ? 1.12 : 1 / 1.12;
  const next = Math.min(3, Math.max(0.08, state.scale * factor));

  state.pan.x = mx - (mx - state.pan.x) * (next / state.scale);
  state.pan.y = my - (my - state.pan.y) * (next / state.scale);
  state.scale = next;
  applyTransform();
  saveLayoutSoon();
}, { passive: false });

// Only accept our own drag type; text selections must never become cards.
canvas.addEventListener('dragover', ev => {
  if (!ev.dataTransfer.types.includes(DRAG_TYPE)) return;
  ev.preventDefault();
  ev.dataTransfer.dropEffect = 'copy';
});
canvas.addEventListener('drop', ev => {
  const path = ev.dataTransfer.getData(DRAG_TYPE);
  if (!path) return;
  ev.preventDefault();
  const p = toWorld(ev.clientX, ev.clientY);
  openFile(path, { x: p.x, y: p.y });
});

// ── placement ────────────────────────────────────────────────────────────

function nextSpot() {
  const rect = canvas.getBoundingClientRect();
  const p = toWorld(rect.left + canvas.clientWidth / 2, rect.top + canvas.clientHeight / 2);
  const step = (state.cards.length % 8) * 28;
  return { x: p.x - 320 + step, y: p.y - 220 + step };
}

function openFile(path, opts = {}) {
  const spot = opts.x !== undefined ? {} : nextSpot();
  const card = createCard(path, { ...spot, ...opts });
  applyLOD(); // a card created while zoomed out otherwise renders as text
  onCardsChanged();
  return card;
}

/** True when the event asks for a separate card rather than reusing one. */
function wantsNewCard(ev) { return ev.shiftKey; }

function arrange() {
  const cards = state.selection.size ? selectedCards() : state.cards;
  if (!cards.length) return;

  const sorted = [...cards].sort((a, b) => a.path.localeCompare(b.path));
  const gap = 32;
  const colHeight = Math.max(760, canvas.clientHeight / Math.max(state.scale, 0.2));
  const originX = Math.min(...sorted.map(c => c.x));
  const originY = Math.min(...sorted.map(c => c.y));

  let x = 0, y = 0, colWidth = 0;
  for (const card of sorted) {
    const h = card.collapsed ? card.el.offsetHeight : card.h;
    if (y > 0 && y + h > colHeight) {
      x += colWidth + gap;
      y = 0;
      colWidth = 0;
    }
    card.x = originX + x;
    card.y = originY + y;
    card.el.style.left = `${card.x}px`;
    card.el.style.top = `${card.y}px`;
    y += h + gap;
    colWidth = Math.max(colWidth, card.w);
  }
  if (!state.selection.size) fit();
  onGeometryChange();
}

/** Aligns the selected cards into one column, left edges flush. */
function alignSelection() {
  const cards = selectedCards();
  if (cards.length < 2) return;
  const sorted = [...cards].sort((a, b) => a.y - b.y);
  const x = Math.min(...cards.map(c => c.x));
  let y = sorted[0].y;
  for (const card of sorted) {
    card.x = x;
    card.y = y;
    card.el.style.left = `${x}px`;
    card.el.style.top = `${y}px`;
    y += (card.collapsed ? card.el.offsetHeight : card.h) + 28;
  }
  onGeometryChange();
}

function fit() {
  if (!state.cards.length) return;
  let x1 = Infinity, y1 = Infinity, x2 = -Infinity, y2 = -Infinity;
  for (const c of state.cards) {
    const h = c.collapsed ? c.el.offsetHeight : c.h;
    x1 = Math.min(x1, c.x); y1 = Math.min(y1, c.y);
    x2 = Math.max(x2, c.x + c.w); y2 = Math.max(y2, c.y + h);
  }
  const pad = 48;
  const sx = (canvas.clientWidth - pad * 2) / Math.max(1, x2 - x1);
  const sy = (canvas.clientHeight - pad * 2) / Math.max(1, y2 - y1);
  state.scale = Math.min(2, Math.max(0.05, Math.min(sx, sy)));
  state.pan.x = (canvas.clientWidth - (x2 - x1) * state.scale) / 2 - x1 * state.scale;
  state.pan.y = (canvas.clientHeight - (y2 - y1) * state.scale) / 2 - y1 * state.scale;
  applyTransform();
  saveLayoutSoon();
}

// ── minimap ──────────────────────────────────────────────────────────────

function drawMinimap() {
  const ctx = minimap.getContext('2d');
  const W = minimap.width, H = minimap.height;
  ctx.clearRect(0, 0, W, H);
  if (!state.cards.length) { minimap.classList.add('hidden'); return; }
  minimap.classList.remove('hidden');

  let x1 = Infinity, y1 = Infinity, x2 = -Infinity, y2 = -Infinity;
  const boxes = state.cards.map(c => {
    const h = c.collapsed ? c.el.offsetHeight : c.h;
    x1 = Math.min(x1, c.x); y1 = Math.min(y1, c.y);
    x2 = Math.max(x2, c.x + c.w); y2 = Math.max(y2, c.y + h);
    return { x: c.x, y: c.y, w: c.w, h, sel: state.selection.has(c.id) };
  });

  const vx1 = -state.pan.x / state.scale;
  const vy1 = -state.pan.y / state.scale;
  const vx2 = vx1 + canvas.clientWidth / state.scale;
  const vy2 = vy1 + canvas.clientHeight / state.scale;
  x1 = Math.min(x1, vx1); y1 = Math.min(y1, vy1);
  x2 = Math.max(x2, vx2); y2 = Math.max(y2, vy2);

  const pad = 6;
  const s = Math.min((W - pad * 2) / (x2 - x1 || 1), (H - pad * 2) / (y2 - y1 || 1));
  const tx = v => pad + (v - x1) * s;
  const ty = v => pad + (v - y1) * s;

  for (const b of boxes) {
    ctx.fillStyle = b.sel ? '#67c9d6' : '#39424e';
    ctx.fillRect(tx(b.x), ty(b.y), Math.max(2, b.w * s), Math.max(2, b.h * s));
  }
  ctx.strokeStyle = '#67c9d6';
  ctx.lineWidth = 1;
  ctx.strokeRect(tx(vx1), ty(vy1), (vx2 - vx1) * s, (vy2 - vy1) * s);

  minimap._transform = { x1, y1, s, pad };
}

minimap.addEventListener('click', ev => {
  const t = minimap._transform;
  if (!t) return;
  const r = minimap.getBoundingClientRect();
  const wx = (ev.clientX - r.left - t.pad) / t.s + t.x1;
  const wy = (ev.clientY - r.top - t.pad) / t.s + t.y1;
  state.pan.x = canvas.clientWidth / 2 - wx * state.scale;
  state.pan.y = canvas.clientHeight / 2 - wy * state.scale;
  applyTransform();
  saveLayoutSoon();
});

// ── layout persistence ───────────────────────────────────────────────────

/** Bumped when the layout shape changes incompatibly. */
const LAYOUT_VERSION = 1;

function snapshot() {
  return {
    v: LAYOUT_VERSION,
    pan: state.pan,
    scale: state.scale,
    diffMode: state.diffMode,
    lodStyle: state.lodStyle,
    fontScale: state.fontScale,
    viewed: [...state.viewed],
    treeCollapsed: [...state.treeCollapsed],
    cards: state.cards.map(c => ({
      path: c.path, x: c.x, y: c.y, w: c.w, h: c.h,
      collapsed: c.collapsed, context: c.context, view: c.view,
      fontScale: c.fontScale || 0, id: c.id,
    })),
    arrows: state.arrows,
  };
}

const saveLayoutSoon = debounce(() => {
  api.saveLayout(snapshot()).catch(() => {});
}, 700);

async function restoreLayout() {
  let saved = null;
  try {
    saved = (await api.layout()).layout;
  } catch { /* a missing layout is not worth surfacing */ }
  if (!saved) return false;
  // A layout from a build with a different shape is discarded rather than
  // partially applied; it costs one arrangement, not a confusing half-restore.
  if ((saved.v || 0) !== LAYOUT_VERSION) return false;

  // Settings are restored even with no cards. Closing the last card wrote
  // `cards: []`, and bailing here threw away the reviewed-file marks — the one
  // piece of state that cannot be reconstructed — along with the zoom style
  // and font size.
  state.pan = saved.pan || state.pan;
  state.scale = saved.scale || 1;
  state.viewed = new Set(saved.viewed || []);
  state.treeCollapsed = new Set(saved.treeCollapsed || []);
  if (saved.diffMode) setDiffMode(saved.diffMode, false);
  if (saved.lodStyle) setLodStyle(saved.lodStyle, false);
  if (saved.fontScale) setFontScale(saved.fontScale, false);

  const remap = new Map();
  for (const c of saved.cards || []) {
    const card = createCard(c.path, {
      x: c.x, y: c.y, w: c.w, h: c.h, context: c.context ?? 3,
      view: c.view ?? 'auto', fontScale: c.fontScale || 0, duplicate: true,
    });
    if (c.collapsed) setCollapsed(card, true);
    remap.set(c.id, card.id);
  }
  loadArrows((saved.arrows || [])
    .map(a => ({ ...a, from: remap.get(a.from), to: remap.get(a.to) }))
    .filter(a => a.from !== undefined && a.to !== undefined));

  applyTransform();
  onGeometryChange();
  return (saved.cards || []).length > 0;
}

// ── panes ────────────────────────────────────────────────────────────────

function refreshChangeTree() {
  renderTree(document.getElementById('tree'),
    state.changes.map(c => ({ path: c.path, change: c })),
    document.getElementById('filter').value, openFile);
}

function refreshFileTree() {
  renderTree(document.getElementById('file-tree'),
    allPaths.map(p => ({ path: p, change: state.byPath.get(p) || null })),
    document.getElementById('file-filter').value, openFile);
}

function setupTabs() {
  const tabs = [
    ['tab-changes', 'pane-changes'],
    ['tab-files', 'pane-files'],
    ['tab-search', 'pane-search'],
  ];
  for (const [tabId, paneId] of tabs) {
    document.getElementById(tabId).addEventListener('click', async () => {
      for (const [t, p] of tabs) {
        document.getElementById(t).classList.toggle('active', t === tabId);
        document.getElementById(p).classList.toggle('active', p === paneId);
      }
      if (paneId === 'pane-files' && !allPaths.length) {
        allPaths = (await api.tree()).paths;
        refreshFileTree();
      }
    });
  }
}

function setupSearch() {
  const input = document.getElementById('grep');
  const status = document.getElementById('grep-status');
  const results = document.getElementById('grep-results');

  input.addEventListener('keydown', async ev => {
    if (ev.key !== 'Enter') return;
    const q = input.value.trim();
    if (!q) return;
    status.textContent = 'searching…';
    results.replaceChildren();
    try {
      const { hits } = await api.grep(q, document.getElementById('grep-case').checked);
      status.textContent = hits.length ? `${hits.length} hit${hits.length === 1 ? '' : 's'}` : 'no matches';
      const frag = document.createDocumentFragment();
      for (const hit of hits) {
        const row = el('div', 'grep-hit');
        row.append(
          el('div', 'grep-path', `${hit.path}:${hit.line}`),
          el('div', 'grep-text', hit.text.slice(0, 200)),
        );
        // Open at the hit, not the top of the file: that is the whole point.
        row.addEventListener('click', () => openFile(hit.path, { line: hit.line }));
        frag.appendChild(row);
      }
      results.appendChild(frag);
    } catch (err) {
      status.textContent = String(err.message || err);
    }
  });
}

function setDiffMode(mode, rebuild = true) {
  state.diffMode = mode;
  // Paint the button state first; the rebuild can take a frame with several
  // large cards open, and a button that looks dead invites a second click.
  document.getElementById('mode-unified').classList.toggle('active', mode === 'unified');
  document.getElementById('mode-split').classList.toggle('active', mode === 'split');
  if (rebuild) {
    requestAnimationFrame(() => {
      rebuildAll();
      saveLayoutSoon();
    });
  }
}

/**
 * Code font size.
 *
 * Zooming out shrinks everything, so working permanently zoomed out needs a
 * bigger base size rather than a different rendering. Row height moves with
 * it, because row virtualisation depends on every row being exactly one
 * known height.
 */
function setFontScale(k, apply = true) {
  state.fontScale = Math.min(2.5, Math.max(0.6, k));
  const root = document.documentElement.style;
  root.setProperty('--row-h', `${rowH()}px`);
  root.setProperty('--code-font', `${(BASE_FONT * state.fontScale).toFixed(2)}px`);
  if (apply) {
    relayoutAll();
    saveLayoutSoon();
    toast(`code font ${Math.round(state.fontScale * 100)}%`);
  }
}

/**
 * Adjusts the font of the selected cards, or of everything when nothing is
 * selected — the same rule the rest of the toolbar actions follow.
 */
function stepFontScale(delta) {
  const cards = selectedCards();
  if (!cards.length) {
    setFontScale(state.fontScale + delta);
    return;
  }
  for (const c of cards) {
    c.fontScale = Math.min(2.5, Math.max(0.6, (c.fontScale || state.fontScale) + delta));
    applyFontScale(c);
  }
  toast(`${cards.length} card${cards.length === 1 ? '' : 's'}: font ` +
        `${Math.round((cards[0].fontScale) * 100)}%`);
  saveLayoutSoon();
}

/** Clears per-card overrides, or resets the global scale. */
function resetFontScale() {
  const cards = selectedCards();
  if (!cards.length) { setFontScale(1); return; }
  for (const c of cards) { c.fontScale = 0; applyFontScale(c); }
  toast('font reset to the global size');
  saveLayoutSoon();
}

const LOD_STYLES = [
  { value: 'texture', label: 'shape', title: 'Code shape: indentation and line length, coloured by change' },
  { value: 'bars', label: 'bars', title: 'How much changed, and roughly where' },
  { value: 'plain', label: 'off', title: 'Name and counts only' },
  { value: 'text', label: 'text', title: 'Never substitute: keep the code, just small' },
];

function setLodStyle(value, redraw = true) {
  state.lodStyle = value;
  const opt = LOD_STYLES.find(o => o.value === value) || LOD_STYLES[0];
  document.getElementById('btn-lod').textContent = opt.label;
  document.getElementById('btn-lod').title = `Zoomed-out cards: ${opt.title} (z)`;
  if (redraw) {
    applyLOD();      // 'text' opts out of the substitution entirely
    redrawStrips();
    saveLayoutSoon();
  }
}

function cycleLodStyle() {
  const i = LOD_STYLES.findIndex(o => o.value === state.lodStyle);
  setLodStyle(LOD_STYLES[(i + 1) % LOD_STYLES.length].value);
  toast(LOD_STYLES.find(o => o.value === state.lodStyle).title);
}

/** Moves the active card to its next or previous change. */
function stepChange(dir) {
  const cards = state.selection.size ? selectedCards()
    : state.lastCard && state.cards.includes(state.lastCard) ? [state.lastCard]
    : state.cards.slice(0, 1);
  if (!cards.length) { toast('open a card first'); return; }
  for (const c of cards) {
    const at = jumpChange(c, dir);
    if (!at) toast(`${c.path}: no changes to step through`);
    else if (cards.length === 1) toast(`change ${at.index} of ${at.total}`);
  }
}

let allCollapsed = false;
function collapseAll() {
  allCollapsed = !allCollapsed;
  for (const card of state.cards) setCollapsed(card, allCollapsed);
  document.getElementById('btn-collapse').textContent = allCollapsed ? 'Expand' : 'Collapse';
  onGeometryChange();
}

/**
 * Opens the declaration of a Ctrl-clicked identifier and links back to the
 * call site, so following a chain of calls leaves a visible trail.
 */
/**
 * Offers the alternatives when resolution is not certain.
 *
 * Without a type checker some names genuinely cannot be resolved, so a wrong
 * best-guess must never be a dead end: the list stays on screen and any
 * candidate can be opened instead.
 */
function showCandidates(sourceCard, res, name, qual) {
  document.getElementById('def-picker')?.remove();

  const box = el('div');
  box.id = 'def-picker';
  const label = qual ? `${qual}.${name}` : name;
  box.appendChild(el('div', 'picker-head', res.confidence === 'guess'
    ? `${label} — type could not be determined; pick the definition:`
    : `${label} — resolved by ${res.confidence}; other candidates:`));

  for (const def of [res.def, ...(res.others || [])]) {
    const row = el('button', 'picker-row');
    row.appendChild(el('span', 'picker-kind', def.kind));
    row.appendChild(el('span', 'picker-name', (def.recv ? def.recv + '.' : '') + def.name));
    row.appendChild(el('span', 'picker-path', `${def.path}:${def.line}`));
    row.addEventListener('click', () => {
      const target = openFile(def.path, { line: def.line });
      if (target && target !== sourceCard) addArrow(sourceCard, target, name);
      box.remove();
      onGeometryChange();
    });
    box.appendChild(row);
  }

  const close = el('button', 'picker-close', 'dismiss');
  close.addEventListener('click', () => box.remove());
  box.appendChild(close);

  document.body.appendChild(box);
  setTimeout(() => {
    document.addEventListener('pointerdown', function once(ev) {
      if (!box.contains(ev.target)) { box.remove(); document.removeEventListener('pointerdown', once); }
    });
  }, 0);
}

async function jumpToDefinition(sourceCard, name, qual, line = 0, separate = false) {
  // Acknowledge immediately. The first lookup builds the declaration index,
  // which on a large repository takes long enough that silence reads as
  // "nothing happened" and invites a second click.
  toast(`resolving ${qual ? qual + '.' : ''}${name}…`);

  let res;
  try {
    res = await api.def(name, qual, sourceCard.path, line);
  } catch (err) {
    toast(String(err.message || err));
    return;
  }
  const def = res.def;

  // Resolution is heuristic without a type checker, so say how sure it is
  // rather than silently opening the wrong file.
  const note = {
    'exact-package': '',
    'same-package': '',
    unique: '',
    'package-name': ' — matched by package name',
    'receiver-type': '',
    guess: ` — best guess, ${(res.others || []).length} other candidate(s)`,
  }[res.confidence] ?? '';

  // A guess means the type could not be worked out — a struct field, an
  // interface value, something from another package. Opening the best-ranked
  // candidate there is worse than useless: it looks like an answer. Say so and
  // let the choice be made explicitly.
  if (res.confidence === 'guess') {
    toast(`cannot resolve ${qual ? qual + '.' : ''}${name} — choose the definition`);
    showCandidates(sourceCard, res, name, qual);
    return;
  }

  const target = openFile(def.path, { line: def.line, duplicate: separate });
  if (target && target !== sourceCard) {
    addArrow(sourceCard, target, name);
  }
  toast(`${def.kind} ${def.recv ? def.recv + '.' : ''}${def.name} → ${def.path}:${def.line}${note}`);
  onGeometryChange();

  // Resolved, but not beyond doubt: offer the alternatives alongside.
  if (res.ambiguous || res.confidence === 'package-name') {
    showCandidates(sourceCard, res, name, qual);
  }
}

/** Asks the server which open Go files import one another, and links them. */
async function drawImportArrows() {
  const paths = state.cards.map(c => c.path).filter(p => p.endsWith('.go'));
  if (paths.length < 2) {
    toast('open at least two Go files first');
    return;
  }
  try {
    const { edges } = await api.imports(paths);
    const added = linkImports(edges.map(e => [e.from, e.to]));
    toast(added ? `linked ${added} import${added === 1 ? '' : 's'}` : 'no imports between the open files');
  } catch (err) {
    toast(String(err.message || err));
  }
}

function setupToolbar() {
  document.getElementById('mode-unified').addEventListener('click', () => setDiffMode('unified'));
  document.getElementById('mode-split').addEventListener('click', () => setDiffMode('split'));
  document.getElementById('btn-fit').addEventListener('click', fit);
  document.getElementById('btn-layout').addEventListener('click', arrange);
  document.getElementById('btn-collapse').addEventListener('click', collapseAll);
  document.getElementById('btn-lod').addEventListener('click', cycleLodStyle);
  document.getElementById('btn-clear').addEventListener('click', () => {
    if (state.cards.length && !confirm(`Remove all ${state.cards.length} cards?`)) return;
    for (const card of [...state.cards]) removeCard(card);
    state.arrows = [];
    onCardsChanged();
  });

  document.getElementById('btn-sidebar').addEventListener('click', () => {
    document.body.classList.toggle('sidebar-hidden');
  });

  document.getElementById('sel-align').addEventListener('click', alignSelection);
  document.getElementById('sel-collapse').addEventListener('click', () => {
    const cards = selectedCards();
    const target = !cards.every(c => c.collapsed);
    for (const c of cards) setCollapsed(c, target);
    onGeometryChange();
  });
  document.getElementById('sel-close').addEventListener('click', () => {
    for (const c of selectedCards()) removeCard(c);
    onCardsChanged();
  });

  document.getElementById('filter').addEventListener('input', refreshChangeTree);
  document.getElementById('file-filter').addEventListener('input', refreshFileTree);
  window.addEventListener('dc:tree-refresh', () => {
    refreshChangeTree();
    refreshFileTree();
    saveLayoutSoon(); // directory collapse is part of the arrangement
  });
}

function setupKeys() {
  document.addEventListener('keydown', ev => {
    const typing = ev.target.tagName === 'INPUT' || ev.target.tagName === 'TEXTAREA';

    if (ev.key === 'Escape') {
      ev.target.blur?.();
      state.selectedArrow = null;
      clearSelection();
      renderArrows();
      return;
    }
    if (typing) return;

    if ((ev.ctrlKey || ev.metaKey) && ev.key === 'a') {
      ev.preventDefault();
      selectAll();
      return;
    }
    if (ev.key === 'Delete' || ev.key === 'Backspace') {
      if (deleteSelectedArrow()) { ev.preventDefault(); return; }
      const cards = selectedCards();
      if (cards.length) {
        ev.preventDefault();
        for (const c of cards) removeCard(c);
        onCardsChanged();
      }
      return;
    }
    if (ev.ctrlKey || ev.metaKey || ev.altKey) return;

    switch (ev.key) {
      case '/': ev.preventDefault(); document.getElementById('filter').focus(); break;
      case 'f': fit(); break;
      case 'a': arrange(); break;
      case 'c': collapseAll(); break;
      case 'u': setDiffMode('unified'); break;
      case 's': setDiffMode('split'); break;
      case 'i': drawImportArrows(); break;
      case 'z': cycleLodStyle(); break;
      case 'n': stepChange(1); break;
      case 'N': stepChange(-1); break;
      case '+': case '=': ev.preventDefault(); stepFontScale(0.1); break;
      case '-': case '_': ev.preventDefault(); stepFontScale(-0.1); break;
      case '0': ev.preventDefault(); resetFontScale(); break;
      case '\\': document.body.classList.toggle('sidebar-hidden'); break;
    }
  });
}

/**
 * `?debug=1` overlay: reports what each click actually lands on.
 *
 * Clickability problems here turned out to depend on display scaling, which is
 * invisible from a screenshot alone. This makes one screenshot enough to tell
 * whether an event reached the button you aimed at.
 */
function setupDebugOverlay() {
  if (!new URLSearchParams(location.search).has('debug')) return;

  const panel = el('div');
  panel.style.cssText =
    'position:fixed;right:8px;bottom:8px;z-index:99999;background:#000;color:#6f6;' +
    'font:11px ui-monospace,monospace;padding:8px;border:1px solid #6f6;' +
    'white-space:pre;pointer-events:none;max-width:60vw';
  document.body.appendChild(panel);

  // Outline where the buttons really are. If the outlines sit exactly on the
  // visible buttons but clicking them reports nothing, the clicks are being
  // swallowed before they reach the page — by something outside the browser,
  // not by this layout.
  const outlines = el('div');
  outlines.style.cssText = 'position:fixed;inset:0;z-index:99998;pointer-events:none';
  document.body.appendChild(outlines);
  const drawOutlines = () => {
    outlines.replaceChildren();
    for (const b of document.querySelectorAll('#toolbar button')) {
      const r = b.getBoundingClientRect();
      const box = el('div');
      box.style.cssText =
        `position:fixed;left:${r.left}px;top:${r.top}px;width:${r.width}px;` +
        `height:${r.height}px;border:1px solid #f0f;pointer-events:none`;
      outlines.appendChild(box);
    }
  };
  drawOutlines();
  window.addEventListener('resize', drawOutlines);
  setInterval(drawOutlines, 1000);

  let clicks = 0;

  const describe = node =>
    !node ? 'null'
      : (node.id ? '#' + node.id : node.tagName.toLowerCase()) +
        (node.className && typeof node.className === 'string' ? '.' + node.className.split(' ')[0] : '');

  const report = ev => {
    clicks++;
    const hit = document.elementFromPoint(ev.clientX, ev.clientY);
    const btn = hit && hit.closest ? hit.closest('button') : null;
    panel.textContent = [
      `events seen: ${clicks}   (if this does not rise, the click never reached the page)`,
      `dpr=${window.devicePixelRatio}  viewport=${window.innerWidth}x${window.innerHeight}`,
      `zoom=${(window.visualViewport ? window.visualViewport.scale : 1).toFixed(2)}` +
        `  toolbar-h=${getComputedStyle(document.documentElement).getPropertyValue('--toolbar-h').trim()}`,
      `click at ${Math.round(ev.clientX)},${Math.round(ev.clientY)}`,
      `  target      ${describe(ev.target)}`,
      `  elementFrom ${describe(hit)}`,
      `  button      ${btn ? '#' + btn.id : 'none'}`,
    ].join('\n');
  };
  window.addEventListener('pointerdown', report, true);
  window.addEventListener('click', report, true);
}

/**
 * Surfaces a fatal script error in the DOM.
 *
 * A module that throws on load leaves a blank page and a message only in the
 * console, where no test and no bug report can see it. Writing it into the
 * document makes the failure visible to a reader and assertable by a headless
 * browser check.
 */
function reportFatalErrors() {
  const show = message => {
    let box = document.getElementById('dc-error');
    if (!box) {
      box = el('div');
      box.id = 'dc-error';
      document.body.appendChild(box);
    }
    box.textContent = `diffcanvas failed: ${message}`;
  };
  window.addEventListener('error', ev => show(ev.message || String(ev.error)));
  window.addEventListener('unhandledrejection', ev =>
    show((ev.reason && ev.reason.message) || String(ev.reason)));
}

// ── start ────────────────────────────────────────────────────────────────

async function main() {
  reportFatalErrors();
  setCardListener(onGeometryChange, onCardsChanged);
  window.addEventListener('dc:jump', ev =>
    jumpToDefinition(ev.detail.card, ev.detail.name, ev.detail.qual, ev.detail.line, ev.detail.separate));

  // Show the jump cursor while the modifier is held.
  const setJumpCursor = on => document.body.classList.toggle('jumping', on);
  window.addEventListener('keydown', e => { if (e.ctrlKey || e.metaKey || e.altKey) setJumpCursor(true); });
  window.addEventListener('keyup', () => setJumpCursor(false));
  window.addEventListener('blur', () => setJumpCursor(false));
  setSelectionListener(() => { syncSelectionBar(); drawMinimap(); });
  initArrows(onGeometryChange);
  setupTabs();
  setupSearch();
  setupToolbar();
  setupKeys();
  trackToolbarHeight();
  setupDebugOverlay();
  setLodStyle(state.lodStyle, false);
  setFontScale(state.fontScale, false);

  try {
    state.meta = await api.meta();
  } catch (err) {
    document.getElementById('spec-label').textContent = `error: ${err.message || err}`;
    return;
  }

  const { spec, repo, files, adds, dels } = state.meta;
  document.getElementById('spec-label').textContent = spec.label;
  document.getElementById('spec-meta').textContent =
    `${repo}${spec.author ? ` · ${spec.author}` : ''}${spec.date ? ` · ${spec.date}` : ''}`;
  document.title = `diffcanvas — ${spec.label}`;

  const { changes } = await api.changes();
  state.changes = changes;
  state.byPath = new Map(changes.map(c => [c.path, c]));
  document.getElementById('change-stats').textContent =
    `${files} files · +${adds} · −${dels}`;

  refreshChangeTree();
  applyTransform();

  // Warm the Go declaration index in the background so the first ctrl-click is
  // instant. Failure is irrelevant: it is a cache, and the real lookup rebuilds
  // it if this never lands.
  if (changes.some(c => c.path.endsWith('.go'))) {
    setTimeout(() => { api.def('main', '', '').catch(() => {}); }, 1200);
  }

  if (await restoreLayout()) toast('restored your previous layout');
  // restoreLayout repopulates state.viewed, and only a full rebuild renders
  // the reviewed strikethroughs.
  refreshChangeTree();
  refreshSelection();
  onCardsChanged();
}

main().catch(err => {
  document.getElementById('spec-label').textContent = `error: ${err.message || err}`;
  const box = document.createElement('div');
  box.id = 'dc-error';
  box.textContent = `diffcanvas failed: ${err.message || err}`;
  document.body.appendChild(box);
});
