// Cards: one file rendered on the canvas.
//
// Scale rests on two things. Rows are virtualised, so a 3000-line file costs
// the same as a 30-line one. Below a zoom threshold the body is replaced by a
// density strip, which is both the cheap render path and the feature that lets
// a whole change be read at a glance.
//
// Note there is no diff/whole-file mode. Whole file is simply maximum context,
// which is why side-by-side keeps working at every step of the continuum.

import { api } from './api.js';
import { state, el, ROW_H, LOD_SCALE, splitPath } from './store.js';
import { select, isSelected, selectedCards } from './selection.js';

const OVERSCAN = 6;

/**
 * Per-card view. 'auto' follows the global unified/split toggle, except that an
 * added file has nothing on the old side, so side-by-side would waste half the
 * card on an empty column.
 */
const VIEW_OPTIONS = [
  { value: 'auto', label: 'auto', title: 'Follow the toolbar; new files show only the new side' },
  { value: 'unified', label: 'uni', title: 'Unified, this card only' },
  { value: 'split', label: 'split', title: 'Side by side, this card only' },
  { value: 'new', label: 'new', title: 'New side only — no deletions, no empty column' },
];

/** The context continuum. "all" is a context wide enough to swallow any file. */
const CONTEXT_STEPS = [
  { label: '3', value: 3 },
  { label: '10', value: 10 },
  { label: '40', value: 40 },
  { label: 'all', value: 100000 },
];

let onChange = () => {};
export function setCardListener(fn) { onChange = fn; }

export function createCard(path, opts = {}) {
  const existing = !opts.duplicate && state.cards.find(c => c.path === path);
  if (existing) {
    focusCard(existing);
    if (opts.line) {
      // The card may still be loading, in which case there are no rows to
      // scroll to yet; hand the line to applyData instead of dropping it.
      if (existing.data) scrollToLine(existing, opts.line);
      else existing.pendingLine = opts.line;
    }
    return existing;
  }

  const card = {
    id: state.nextId++,
    path,
    x: opts.x ?? 0,
    y: opts.y ?? 0,
    w: opts.w ?? 640,
    h: opts.h ?? 440,
    collapsed: false,
    // 'auto' follows the global unified/split setting, except for added files
    // where it drops the empty old-side column. Anything else pins this card.
    view: opts.view ?? 'auto',
    context: opts.context ?? 3,
    pendingLine: opts.line || 0,
    data: null,
    rows: [],
    el: null,
    lod: false,
  };
  state.cards.push(card);
  buildCardDOM(card);
  loadCard(card);
  onChange();
  return card;
}

export function removeCard(card) {
  const i = state.cards.indexOf(card);
  if (i >= 0) state.cards.splice(i, 1);
  state.arrows = state.arrows.filter(a => a.from !== card.id && a.to !== card.id);
  state.selection.delete(card.id);
  card.el?.remove();
  onChange();
}

export function focusCard(card) {
  card.el.style.zIndex = String(topZ());
  card.el.animate(
    [{ boxShadow: '0 0 0 3px rgba(103,201,214,.6)' }, { boxShadow: '0 2px 10px rgba(0,0,0,.35)' }],
    { duration: 650, easing: 'ease-out' },
  );
}

// A monotonically increasing counter. Deriving this from the card count made
// it constant for a given number of cards, so once every card had been clicked
// an older one could never be raised above a newer one again.
let zTop = 10;
function topZ() { return ++zTop; }

function buildCardDOM(card) {
  const node = el('div', 'card');
  node.style.cssText = `left:${card.x}px;top:${card.y}px;width:${card.w}px;height:${card.h}px`;
  node.dataset.id = String(card.id);

  // ── header: quiet by default, actions on hover ──
  const head = el('div', 'card-head');
  const [dir, name] = splitPath(card.path);
  const dot = el('span', 'dot');
  const title = el('div', 'card-title');
  if (dir) title.appendChild(el('span', 'dim', dir));
  title.appendChild(document.createTextNode(name));
  title.title = card.path;
  const counts = el('span', 'card-sub');

  const actions = el('div', 'card-actions');
  const btnDup = iconBtn('⧉', 'Open a second copy', 'dup');
  const btnFold = iconBtn('−', 'Collapse', 'fold');
  const btnClose = iconBtn('×', 'Close', 'close');
  actions.append(btnDup, btnFold, btnClose);
  head.append(dot, title, counts, actions);

  // ── body ──
  const body = el('div', 'card-body');
  const spacer = el('div', 'spacer');
  const rows = el('div', 'rows');
  body.append(spacer, rows);

  const strip = document.createElement('canvas');
  strip.className = 'lod-strip';

  // ── footer: language, then the context continuum ──
  const foot = el('div', 'card-foot');
  const lang = el('span', 'foot-lang', 'loading…');

  const viewSeg = el('div', 'foot-seg');
  viewSeg.appendChild(el('span', 'ctx-label', 'view'));
  for (const opt of VIEW_OPTIONS) {
    const b = el('button', '', opt.label);
    b.title = opt.title;
    b.dataset.view = opt.value;
    b.addEventListener('click', ev => {
      ev.stopPropagation();
      card.view = opt.value;
      card.rows = buildRows(card);
      layoutRows(card);
      syncViewButtons(card);
      onChange();
    });
    viewSeg.appendChild(b);
  }

  const ctxSeg = el('div', 'ctx-seg foot-seg');
  ctxSeg.appendChild(el('span', 'ctx-label', 'context'));
  for (const step of CONTEXT_STEPS) {
    const b = el('button', '', step.label);
    b.title = step.value >= 100000 ? 'Whole file' : `${step.label} lines around each change`;
    b.dataset.ctx = String(step.value);
    b.addEventListener('click', ev => {
      ev.stopPropagation();
      if (card.context === step.value) return;
      card.context = step.value;
      loadCard(card);
    });
    ctxSeg.appendChild(b);
  }
  foot.append(lang, viewSeg, ctxSeg);

  // ── edge ports for arrows ──
  const portL = el('div', 'port left');
  const portR = el('div', 'port right');
  portL.title = portR.title = 'Drag to another card to link them';

  const resize = el('div', 'resize');
  node.append(head, body, strip, foot, portL, portR, resize);
  document.getElementById('cards').appendChild(node);

  Object.assign(card, {
    el: node, headEl: head, bodyEl: body, rowsEl: rows, spacerEl: spacer,
    dotEl: dot, countsEl: counts, langEl: lang, stripEl: strip,
    ctxSegEl: ctxSeg, viewSegEl: viewSeg,
  });

  body.addEventListener('scroll', () => renderRows(card), { passive: true });

  // Ctrl/Cmd-click an identifier to open its declaration, the gesture an IDE
  // already trains into your hands.
  body.addEventListener('click', ev => {
    if (!ev.ctrlKey && !ev.metaKey && !ev.altKey) return;
    const found = identifierAt(ev.clientX, ev.clientY);
    if (!found) return;
    ev.preventDefault();
    ev.stopPropagation();
    window.dispatchEvent(new CustomEvent('dc:jump', {
      detail: { card, name: found.name, qual: found.qual },
    }));
  });

  head.addEventListener('pointerdown', ev => {
    if (ev.target.closest('button')) return;
    startDrag(card, ev);
  });

  btnFold.addEventListener('click', () => { setCollapsed(card, !card.collapsed); onChange(); });
  btnClose.addEventListener('click', () => removeCard(card));
  btnDup.addEventListener('click', () => {
    createCard(card.path, {
      duplicate: true, context: card.context,
      x: card.x + 40, y: card.y + 40, w: card.w, h: card.h,
    });
  });

  for (const port of [portL, portR]) {
    port.addEventListener('pointerdown', ev => {
      ev.stopPropagation();
      ev.preventDefault();
      window.dispatchEvent(new CustomEvent('dc:link-start', { detail: { card, ev } }));
    });
  }

  resize.addEventListener('pointerdown', ev => startResize(card, ev));
  node.addEventListener('pointerdown', () => { node.style.zIndex = String(topZ()); }, true);
}

function iconBtn(glyph, title, act) {
  const b = el('button', 'cbtn', glyph);
  b.title = title;
  b.type = 'button';
  b.dataset.act = act;
  return b;
}

export function setCollapsed(card, collapsed) {
  if (card.collapsed === collapsed) return;
  card.collapsed = collapsed;
  card.el.classList.toggle('collapsed', collapsed);
  card.el.querySelector('[data-act="fold"]').textContent = collapsed ? '+' : '−';
  card.el.style.height = collapsed ? 'auto' : `${card.h}px`;

  // renderRows is a no-op while collapsed, so anything that arrived in the
  // meantime — the initial load of a card restored collapsed, or a rebuild
  // after switching split/unified — has to be drawn now.
  if (!collapsed) {
    card.renderedFirst = card.renderedLast = -1;
    renderRows(card);
  }
}

export function setCardSelected(card, on) {
  card.el.classList.toggle('selected', on);
}

async function loadCard(card) {
  card.langEl.textContent = 'loading…';
  syncContextButtons(card);

  // Clicking through the context steps fires overlapping requests, and they do
  // not necessarily come back in order. Only the newest one may apply.
  const generation = (card.loadGen = (card.loadGen || 0) + 1);
  let data;
  try {
    data = await api.file(card.path, 'diff', card.context);
  } catch (err) {
    if (generation !== card.loadGen) return;
    card.langEl.textContent = 'error';
    card.rows = [{ kind: 'note', text: String(err.message || err) }];
    layoutRows(card);
    return;
  }
  if (generation !== card.loadGen) return;
  card.data = data;
  applyData(card);
}

/** The view this card actually renders in, after resolving 'auto'. */
function effectiveView(card) {
  if (card.view !== 'auto') return card.view;
  if (card.data && card.data.status === 'added') return 'new';
  return state.diffMode;
}

function syncViewButtons(card) {
  const active = effectiveView(card);
  for (const b of card.viewSegEl.querySelectorAll('button')) {
    const v = b.dataset.view;
    // Two different facts, so two different marks: 'on' is what you chose,
    // 'resolved' is what 'auto' currently works out to.
    b.classList.toggle('on', v === card.view);
    b.classList.toggle('resolved', card.view === 'auto' && v === active);
  }
}

function syncContextButtons(card) {
  for (const b of card.ctxSegEl.querySelectorAll('button')) {
    b.classList.toggle('on', Number(b.dataset.ctx) === card.context);
  }
}

function applyData(card) {
  const d = card.data;
  card.dotEl.className = `dot st-${d.status}`;
  card.dotEl.title = d.status + (d.oldPath ? ` from ${d.oldPath}` : '');

  card.countsEl.replaceChildren();
  if (d.mode === 'full') {
    card.countsEl.textContent = `${d.lines ? d.lines.length : 0} lines`;
  } else {
    card.countsEl.append(
      el('span', 'a', `+${d.adds}`), document.createTextNode(' '),
      el('span', 'd', `−${d.dels}`),
    );
  }
  card.langEl.textContent = d.lang + (d.truncated ? ' · truncated' : '');

  // An unchanged file has no hunks, so the context continuum is meaningless.
  card.ctxSegEl.style.display = d.mode === 'full' ? 'none' : '';
  card.viewSegEl.style.display = d.mode === 'full' ? 'none' : '';
  syncContextButtons(card);
  syncViewButtons(card);

  card.rows = buildRows(card);
  layoutRows(card);

  if (card.pendingLine) {
    scrollToLine(card, card.pendingLine);
    card.pendingLine = 0;
  }
  onChange();
}

/** Scrolls a card so a given new-side line sits near the top. */
export function scrollToLine(card, line) {
  const index = card.rows.findIndex(r =>
    (r.kind === 'line' && r.new === line) ||
    (r.kind === 'pair' && r.r && r.r.new === line));
  if (index < 0) return;
  card.bodyEl.scrollTop = Math.max(0, (index - 2) * ROW_H);
  renderRows(card);
}

/** Flattens the payload into a uniform, fixed-height row list. */
function buildRows(card) {
  const d = card.data;
  if (d.binary) return [{ kind: 'note', text: 'binary file — not shown' }];

  const rows = [];
  if (d.mode === 'full') {
    const changed = new Set(d.changed || []);
    for (const l of d.lines || []) {
      rows.push({ kind: 'line', t: ' ', old: l.old, new: l.new, segs: l.segs, changed: changed.has(l.new) });
    }
    if (!rows.length) rows.push({ kind: 'note', text: 'empty file' });
    return rows;
  }

  const view = effectiveView(card);
  for (const h of d.hunks || []) {
    rows.push({ kind: 'hunk', header: h.header, oldStart: h.oldStart, newStart: h.newStart });
    if (view === 'split') {
      // Pairing comes from the server so it agrees with the word-level diff.
      for (const [oi, ni] of h.pairs || []) {
        rows.push({ kind: 'pair', l: oi >= 0 ? h.lines[oi] : null, r: ni >= 0 ? h.lines[ni] : null });
      }
    } else if (view === 'new') {
      // Only the resulting file: no deletions, one gutter, no sign column.
      for (const l of h.lines) {
        if (l.t === '-') continue;
        rows.push({ kind: 'newline', new: l.new, segs: l.segs, added: l.t === '+', noNL: l.noNL });
      }
    } else {
      for (const l of h.lines) rows.push({ kind: 'line', ...l });
    }
  }
  if (!rows.length) rows.push({ kind: 'note', text: 'no textual changes' });
  return rows;
}

function layoutRows(card) {
  card.spacerEl.style.height = `${card.rows.length * ROW_H}px`;
  card.renderedFirst = card.renderedLast = -1;
  renderRows(card);
  drawStrip(card);
}

function renderRows(card) {
  if (card.lod || card.collapsed) return;

  const view = card.bodyEl;
  const first = Math.max(0, Math.floor(view.scrollTop / ROW_H) - OVERSCAN);
  const visible = Math.ceil(view.clientHeight / ROW_H) + OVERSCAN * 2;
  const last = Math.min(card.rows.length, first + visible);

  if (card.renderedFirst === first && card.renderedLast === last) return;
  card.renderedFirst = first;
  card.renderedLast = last;

  const frag = document.createDocumentFragment();
  for (let i = first; i < last; i++) frag.appendChild(renderRow(card.rows[i]));
  card.rowsEl.replaceChildren(frag);
  card.rowsEl.style.transform = `translateY(${first * ROW_H}px)`;
}

function renderRow(row) {
  if (row.kind === 'hunk') return renderHunkRow(row);
  if (row.kind === 'note') return el('div', 'row note', row.text);
  if (row.kind === 'pair') return renderPairRow(row);
  if (row.kind === 'newline') return renderNewRow(row);

  const cls = row.t === '+' ? 'row add'
    : row.t === '-' ? 'row del'
    : row.changed ? 'row changed' : 'row';
  const node = el('div', cls);
  node.append(
    el('span', 'gut', row.old ? String(row.old) : ''),
    el('span', 'gut', row.new ? String(row.new) : ''),
    el('span', 'sign', row.t === ' ' ? '' : row.t),
  );
  const txt = el('span', 'txt');
  renderSegs(txt, row.segs);
  if (row.noNL) txt.appendChild(el('span', 'nonl', ' ↵ no newline at end of file'));
  node.appendChild(txt);
  return node;
}

function renderNewRow(row) {
  const node = el('div', row.added ? 'row changed' : 'row');
  node.appendChild(el('span', 'gut', row.new ? String(row.new) : ''));
  const txt = el('span', 'txt');
  renderSegs(txt, row.segs);
  if (row.noNL) txt.appendChild(el('span', 'nonl', ' ↵ no newline at end of file'));
  node.appendChild(txt);
  return node;
}

function renderHunkRow(row) {
  const node = el('div', 'row hunk');
  node.appendChild(document.createTextNode(`${row.oldStart} → ${row.newStart}`));
  if (row.header) {
    node.appendChild(document.createTextNode('  '));
    node.appendChild(el('span', 'hunk-ctx', row.header));
  }
  return node;
}

function renderPairRow(row) {
  const node = el('div', 'row');
  node.append(sideCell(row.l, 'l'), sideCell(row.r, 'r'));
  return node;
}

function sideCell(line, which) {
  const kind = !line ? 'blank' : line.t === '-' ? 'del' : line.t === '+' ? 'add' : '';
  const cell = el('div', `side ${which} ${kind}`.trim());
  if (!line) return cell;

  const num = which === 'l' ? line.old : line.new;
  cell.append(el('span', 'gut', num ? String(num) : ''));
  const txt = el('span', 'txt');
  renderSegs(txt, line.segs);
  if (line.noNL) txt.appendChild(el('span', 'nonl', ' ↵'));
  cell.appendChild(txt);
  return cell;
}

/**
 * Writes highlighted segments as text.
 *
 * Everything goes through textContent, so file contents can never be parsed as
 * markup. A segment can be both syntax-coloured and word-marked, so the two
 * classes compose rather than replacing one another.
 */
function renderSegs(parent, segs) {
  if (!segs || !segs.length) return;
  for (const s of segs) {
    if (!s.c && !s.w) {
      parent.appendChild(document.createTextNode(s.t));
      continue;
    }
    const cls = [s.c ? `tok-${s.c}` : '', s.w ? 'w' : ''].filter(Boolean).join(' ');
    parent.appendChild(el('span', cls, s.t));
  }
}

const WORD_RE = /[A-Za-z0-9_]/;

/**
 * Finds the identifier under the pointer.
 *
 * Reading it out of the rendered text means the payload needs no per-identifier
 * markup: syntax segments already break on token boundaries, so an identifier
 * always sits wholly inside one text node. The preceding character decides
 * whether there is a package or receiver qualifier.
 */
function identifierAt(clientX, clientY) {
  let node = null;
  let offset = 0;

  if (document.caretRangeFromPoint) {
    const range = document.caretRangeFromPoint(clientX, clientY);
    if (range) { node = range.startContainer; offset = range.startOffset; }
  } else if (document.caretPositionFromPoint) {
    const pos = document.caretPositionFromPoint(clientX, clientY);
    if (pos) { node = pos.offsetNode; offset = pos.offset; }
  }
  if (!node || node.nodeType !== Node.TEXT_NODE) return null;

  // Keywords, strings, numbers and comments are never declarations.
  const parentClass = node.parentElement?.className || '';
  if (/tok-(kw|str|num|com)/.test(parentClass)) return null;

  const text = node.data;
  let i = Math.min(offset, text.length - 1);
  if (i < 0) return null;
  if (!WORD_RE.test(text[i]) && i > 0 && WORD_RE.test(text[i - 1])) i--;
  if (!WORD_RE.test(text[i])) return null;

  let start = i;
  let end = i;
  while (start > 0 && WORD_RE.test(text[start - 1])) start--;
  while (end < text.length && WORD_RE.test(text[end])) end++;
  const name = text.slice(start, end);
  if (!name || /^[0-9]/.test(name)) return null;

  // Look back across the whole row for a `qualifier.` prefix.
  const row = node.parentElement?.closest('.txt');
  let qual = '';
  if (row) {
    const before = rowTextBefore(row, node, start);
    const match = before.match(/([A-Za-z0-9_]+)\s*\.\s*$/);
    if (match) qual = match[1];
  }
  return { name, qual };
}

/** Text of the row up to a given position inside one of its text nodes. */
function rowTextBefore(row, node, offset) {
  const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
  let out = '';
  let n;
  while ((n = walker.nextNode())) {
    if (n === node) return out + n.data.slice(0, offset);
    out += n.data;
  }
  return out;
}

/**
 * Draws the zoomed-out density view.
 *
 * Rows use a fixed pitch rather than stretching to fill the card, so strips are
 * comparable between cards: a twelve-line tweak reads as a short stub beside a
 * file that was rewritten wholesale.
 */
const MAX_BANDS = 14;
const BAND_PITCH = 16;

/**
 * The zoomed-out view of a card.
 *
 * Rows are aggregated into a small number of bands rather than drawn one pixel
 * each. Per-row stripes turned a screenful of cards into interleaved red and
 * green noise — a rainbow, not a shape. Each band is instead a single quiet bar
 * whose green and red widths are the proportion of additions and deletions in
 * that slice of the file, so what you read at a glance is "how much, and
 * where", which is all this view is for.
 */
function drawStrip(card) {
  const canvas = card.stripEl;
  const w = Math.max(40, Math.round(card.w));
  const h = Math.max(20, Math.round(card.h - 30));
  canvas.width = w;
  canvas.height = h;

  const ctx = canvas.getContext('2d');
  ctx.fillStyle = '#171b21';
  ctx.fillRect(0, 0, w, h);

  const rows = card.rows;
  if (!rows.length) return;

  const bands = Math.max(1, Math.min(MAX_BANDS, Math.floor(h / BAND_PITCH), rows.length));
  const bandH = Math.min(BAND_PITCH, h / bands);
  const pad = 12;
  const usable = w - pad * 2;

  // A single quiet spine marks the file's extent. Drawing a background bar per
  // band instead is what made this read as stripes.
  ctx.fillStyle = '#232a33';
  ctx.fillRect(pad, 0, 2, Math.round(bands * (h / bands)));

  for (let b = 0; b < bands; b++) {
    const from = Math.floor((b * rows.length) / bands);
    const to = Math.max(from + 1, Math.floor(((b + 1) * rows.length) / bands));

    let adds = 0;
    let dels = 0;
    for (let i = from; i < to; i++) {
      const r = rows[i];
      if (r.kind === 'line' || r.kind === 'newline') {
        if (r.t === '+' || r.added || r.changed) adds++;
        else if (r.t === '-') dels++;
      } else if (r.kind === 'pair') {
        if (r.r && r.r.t === '+') adds++;
        if (r.l && r.l.t === '-') dels++;
      }
    }

    if (adds === 0 && dels === 0) continue; // untouched stretch: draw nothing

    const total = to - from;
    const y = Math.round(b * (h / bands));
    const barH = Math.max(3, bandH - 6);
    const addW = Math.round((adds / total) * usable);
    const delW = Math.round((dels / total) * usable);

    if (addW > 0) {
      ctx.fillStyle = '#3f8f52';
      ctx.fillRect(pad, y, addW, barH);
    }
    if (delW > 0) {
      ctx.fillStyle = '#b8463f';
      ctx.fillRect(pad + addW, y, delW, barH);
    }
  }
}

export function applyLOD() {
  const lod = state.scale < LOD_SCALE;
  for (const card of state.cards) {
    if (card.lod !== lod) {
      card.lod = lod;
      card.el.classList.toggle('lod', lod);
      if (!lod) {
        clearLODLabel(card);
        card.renderedFirst = card.renderedLast = -1;
        renderRows(card);
      }
    }
    if (lod) sizeLODLabel(card);
  }
}

/**
 * Keeps a zoomed-out card's title legible.
 *
 * The world transform shrinks everything, so at 30% the density strips are
 * readable shapes attached to unreadable names — which tells you a change
 * happened but not where. Scaling the header's type inversely to the zoom
 * holds it at a constant on-screen size, so the overview stays navigable.
 */
function sizeLODLabel(card) {
  const k = 1 / Math.max(state.scale, 0.04);
  const head = card.headEl.style;
  head.fontSize = `${11 * k}px`;
  head.padding = `${6 * k}px ${8 * k}px`;
  head.gap = `${7 * k}px`;
  head.borderBottomWidth = `${k}px`;
  card.dotEl.style.width = card.dotEl.style.height = `${6 * k}px`;
}

function clearLODLabel(card) {
  const head = card.headEl.style;
  head.fontSize = head.padding = head.gap = head.borderBottomWidth = '';
  card.dotEl.style.width = card.dotEl.style.height = '';
}

/** Rebuilds every card after a global unified/split switch. */
export function rebuildAll() {
  for (const card of state.cards) {
    if (!card.data) continue;
    card.rows = buildRows(card);
    layoutRows(card);
    syncViewButtons(card); // 'auto' cards follow the global toggle
  }
}

// ── dragging and resizing ────────────────────────────────────────────────

function startDrag(card, ev) {
  ev.preventDefault();

  // Dragging an unselected card selects it first, so a drag always moves
  // exactly what the outline says it will.
  if (!isSelected(card)) select([card], ev.shiftKey);

  const moving = selectedCards();
  const origins = moving.map(c => ({ c, x: c.x, y: c.y }));
  const startX = ev.clientX, startY = ev.clientY;

  document.body.classList.add('dragging');
  for (const c of moving) c.el.classList.add('dragging');
  card.el.style.zIndex = String(topZ());

  const move = e => {
    const dx = (e.clientX - startX) / state.scale;
    const dy = (e.clientY - startY) / state.scale;
    for (const o of origins) {
      o.c.x = o.x + dx;
      o.c.y = o.y + dy;
      o.c.el.style.left = `${o.c.x}px`;
      o.c.el.style.top = `${o.c.y}px`;
    }
    onChange();
  };
  const up = () => {
    document.body.classList.remove('dragging');
    for (const c of moving) c.el.classList.remove('dragging');
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    onChange();
  };
  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
}

function startResize(card, ev) {
  if (card.collapsed) return; // nothing to resize; would leave an empty box
  ev.preventDefault();
  ev.stopPropagation();
  const startX = ev.clientX, startY = ev.clientY;
  const ow = card.w, oh = card.h;
  document.body.classList.add('dragging');

  const move = e => {
    card.w = Math.max(280, ow + (e.clientX - startX) / state.scale);
    card.h = Math.max(90, oh + (e.clientY - startY) / state.scale);
    card.el.style.width = `${card.w}px`;
    card.el.style.height = `${card.h}px`;
    renderRows(card);
    onChange();
  };
  const up = () => {
    document.body.classList.remove('dragging');
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    drawStrip(card);
    onChange();
  };
  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
}
