// Bootstrap: panes, canvas transform, selection wiring, layout persistence.

import { api } from './api.js';
import { state, el, toast, debounce, DRAG_TYPE } from './store.js';
import { renderTree } from './tree.js';
import {
  createCard, removeCard, setCardListener, applyLOD, rebuildAll, setCollapsed,
} from './card.js';
import {
  initArrows, renderArrows, loadArrows, toWorld, deleteSelectedArrow, linkImports,
  addArrow,
} from './arrows.js';
import {
  select, clearSelection, selectAll, selectedCards, startMarquee,
  setSelectionListener, refreshSelection,
} from './selection.js';

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

function onGeometryChange() {
  renderArrows();
  drawMinimap();
  emptyState.style.display = state.cards.length ? 'none' : 'grid';
  refreshTreeMarks();
  syncSelectionBar();
  saveLayoutSoon();
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
  createCard(path, { x: p.x, y: p.y });
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
  return createCard(path, { ...spot, ...opts });
}

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

function snapshot() {
  return {
    pan: state.pan,
    scale: state.scale,
    diffMode: state.diffMode,
    viewed: [...state.viewed],
    cards: state.cards.map(c => ({
      path: c.path, x: c.x, y: c.y, w: c.w, h: c.h,
      collapsed: c.collapsed, context: c.context, view: c.view, id: c.id,
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
  if (!saved || !saved.cards?.length) return false;

  state.pan = saved.pan || state.pan;
  state.scale = saved.scale || 1;
  state.viewed = new Set(saved.viewed || []);
  if (saved.diffMode) setDiffMode(saved.diffMode, false);

  const remap = new Map();
  for (const c of saved.cards) {
    const card = createCard(c.path, {
      x: c.x, y: c.y, w: c.w, h: c.h, context: c.context ?? 3,
      view: c.view ?? 'auto', duplicate: true,
    });
    if (c.collapsed) setCollapsed(card, true);
    remap.set(c.id, card.id);
  }
  loadArrows((saved.arrows || [])
    .map(a => ({ ...a, from: remap.get(a.from), to: remap.get(a.to) }))
    .filter(a => a.from !== undefined && a.to !== undefined));

  applyTransform();
  onGeometryChange();
  return true;
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
  document.getElementById('mode-unified').classList.toggle('active', mode === 'unified');
  document.getElementById('mode-split').classList.toggle('active', mode === 'split');
  if (rebuild) {
    rebuildAll();
    saveLayoutSoon();
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
async function jumpToDefinition(sourceCard, name, qual) {
  let res;
  try {
    res = await api.def(name, qual, sourceCard.path);
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
    guess: ` — best guess, ${(res.others || []).length} other candidate(s)`,
  }[res.confidence] ?? '';

  const target = openFile(def.path, { line: def.line });
  if (target && target !== sourceCard) {
    addArrow(sourceCard, target, name);
  }
  toast(`${def.kind} ${def.recv ? def.recv + '.' : ''}${def.name} → ${def.path}:${def.line}${note}`);
  onGeometryChange();
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
  document.getElementById('btn-clear').addEventListener('click', () => {
    if (state.cards.length && !confirm(`Remove all ${state.cards.length} cards?`)) return;
    for (const card of [...state.cards]) removeCard(card);
    state.arrows = [];
    onGeometryChange();
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
    onGeometryChange();
  });

  document.getElementById('filter').addEventListener('input', refreshChangeTree);
  document.getElementById('file-filter').addEventListener('input', refreshFileTree);
  window.addEventListener('dc:tree-refresh', () => { refreshChangeTree(); refreshFileTree(); });
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
        onGeometryChange();
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
      case '\\': document.body.classList.toggle('sidebar-hidden'); break;
    }
  });
}

// ── start ────────────────────────────────────────────────────────────────

async function main() {
  setCardListener(onGeometryChange);
  window.addEventListener('dc:jump', ev =>
    jumpToDefinition(ev.detail.card, ev.detail.name, ev.detail.qual));

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

  if (await restoreLayout()) {
    toast('restored your previous layout');
    // restoreLayout repopulates state.viewed, and only a full rebuild renders
    // the reviewed strikethroughs.
    refreshChangeTree();
  }
  refreshSelection();
  onGeometryChange();
}

main().catch(err => {
  document.getElementById('spec-label').textContent = `error: ${err.message || err}`;
});
