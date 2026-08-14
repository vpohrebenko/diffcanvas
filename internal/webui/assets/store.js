// Shared mutable state plus small DOM helpers. Kept in its own module so the
// tree, cards and arrows can all reach it without importing each other.

export const state = {
  meta: null,
  changes: [],
  byPath: new Map(),
  cards: [],
  arrows: [],
  viewed: new Set(),
  selection: new Set(),
  nextId: 1,
  diffMode: 'unified', // or 'split'
  lodStyle: 'texture', // zoomed-out card: texture | bars | plain | text
  fontScale: 1,        // code font multiplier
  pan: { x: 60, y: 30 },
  scale: 1,
  selectedArrow: null,
};

/** Row height at font scale 1. */
export const BASE_ROW_H = 18;
export const BASE_FONT = 11;

/**
 * Current row height in pixels. Virtualisation relies on every row being
 * exactly this tall, so anything that changes the code font must update this
 * and --row-h together.
 */
export function rowH() { return Math.round(BASE_ROW_H * state.fontScale); }

/** Below this world scale, cards render as a density strip instead of text. */
export const LOD_SCALE = 0.4;

/** Private drag type. Using text/plain would make any dropped text selection
 *  look like a file path and spawn a phantom card. */
export const DRAG_TYPE = 'application/x-diffcanvas-path';

export function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/** Splits a path into its directory prefix and file name for two-tone display. */
export function splitPath(path) {
  const i = path.lastIndexOf('/');
  return i < 0 ? ['', path] : [path.slice(0, i + 1), path.slice(i + 1)];
}

let toastTimer = null;
export function toast(message) {
  const node = document.getElementById('toast');
  node.textContent = message;
  node.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { node.hidden = true; }, 2200);
}

/** Debounce that also runs during idle time, used for layout autosave. */
export function debounce(fn, ms) {
  let t = null;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
}
