// Card selection: click, shift-click, and shift-drag marquee.
//
// Selection lives here rather than in card.js because arrows, the toolbar and
// the canvas all need to ask about it.

import { state } from './store.js';

let onChange = () => {};
export function setSelectionListener(fn) { onChange = fn; }

export function isSelected(card) { return state.selection.has(card.id); }

export function selectedCards() {
  return state.cards.filter(c => state.selection.has(c.id));
}

/** Selects the given cards, replacing the selection unless additive. */
export function select(cards, additive = false) {
  if (!additive) state.selection.clear();
  for (const c of cards) {
    if (additive && state.selection.has(c.id)) state.selection.delete(c.id);
    else state.selection.add(c.id);
  }
  syncClasses();
  onChange();
}

export function clearSelection() {
  if (!state.selection.size) return;
  state.selection.clear();
  syncClasses();
  onChange();
}

export function selectAll() {
  select(state.cards);
}

function syncClasses() {
  for (const c of state.cards) {
    c.el?.classList.toggle('selected', state.selection.has(c.id));
  }
}

/** Re-applies selection styling after cards are rebuilt. */
export function refreshSelection() {
  // Drop ids belonging to cards that no longer exist.
  const live = new Set(state.cards.map(c => c.id));
  for (const id of [...state.selection]) {
    if (!live.has(id)) state.selection.delete(id);
  }
  syncClasses();
}

/**
 * Runs a marquee selection.
 *
 * Called by the canvas when a drag starts on empty space with shift held.
 * Plain drags pan instead: in a reading tool the viewport moves far more often
 * than the selection changes.
 */
export function startMarquee(canvas, ev, toWorld, additive) {
  const box = document.getElementById('marquee');
  const rect = canvas.getBoundingClientRect();
  const originX = ev.clientX - rect.left;
  const originY = ev.clientY - rect.top;
  const originWorld = toWorld(ev.clientX, ev.clientY);
  const base = additive ? new Set(state.selection) : new Set();

  document.body.classList.add('dragging');
  canvas.classList.add('selecting');
  box.hidden = false;

  const move = e => {
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    box.style.left = `${Math.min(originX, x)}px`;
    box.style.top = `${Math.min(originY, y)}px`;
    box.style.width = `${Math.abs(x - originX)}px`;
    box.style.height = `${Math.abs(y - originY)}px`;

    const now = toWorld(e.clientX, e.clientY);
    const x1 = Math.min(originWorld.x, now.x), x2 = Math.max(originWorld.x, now.x);
    const y1 = Math.min(originWorld.y, now.y), y2 = Math.max(originWorld.y, now.y);

    state.selection = new Set(base);
    for (const c of state.cards) {
      const h = c.collapsed ? c.el.offsetHeight : c.h;
      // Intersection, not containment: a partly covered card still counts.
      if (c.x < x2 && c.x + c.w > x1 && c.y < y2 && c.y + h > y1) {
        state.selection.add(c.id);
      }
    }
    syncClasses();
    onChange();
  };

  const up = () => {
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    document.body.classList.remove('dragging');
    canvas.classList.remove('selecting');
    box.hidden = true;
    box.style.width = box.style.height = '0px';
    onChange();
  };

  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
}
