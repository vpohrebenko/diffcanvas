// Arrows between cards.
//
// Three things were wrong with the first version and are fixed here:
//   - the curve's control points ignored direction, so a right-to-left arrow
//     folded back through its own source card and buried the arrowhead;
//   - the SVG layer sat under the cards, so any arrow crossing a card vanished;
//   - creation hung off a button in the card header rather than the card edge.
//
// Arrows now draw above the cards, bow along the actual direction of travel,
// and start from ports on the card edges.

import { state, toast } from './store.js';

const SVG_NS = 'http://www.w3.org/2000/svg';
let pathsGroup = null;
let nextArrowId = 1;
let onChange = () => {};

export function initArrows(fn) {
  onChange = fn || (() => {});
  pathsGroup = document.getElementById('arrow-paths');
  window.addEventListener('dc:link-start', ev => startLink(ev.detail.card, ev.detail.ev));
}

export function deleteSelectedArrow() {
  if (state.selectedArrow === null) return false;
  state.arrows = state.arrows.filter(a => a.id !== state.selectedArrow);
  state.selectedArrow = null;
  renderArrows();
  onChange();
  return true;
}

function cardById(id) { return state.cards.find(c => c.id === id); }

function rectOf(card) {
  const h = card.collapsed ? card.el.offsetHeight : card.h;
  return { x: card.x, y: card.y, w: card.w, h, cx: card.x + card.w / 2, cy: card.y + h / 2 };
}

/** Where the straight line toward (tx,ty) leaves the card's border. */
function edgePoint(rect, tx, ty) {
  const dx = tx - rect.cx;
  const dy = ty - rect.cy;
  if (dx === 0 && dy === 0) return { x: rect.cx, y: rect.cy };
  const scale = Math.min(
    Math.abs(dx) < 1e-6 ? Infinity : (rect.w / 2) / Math.abs(dx),
    Math.abs(dy) < 1e-6 ? Infinity : (rect.h / 2) / Math.abs(dy),
  );
  return { x: rect.cx + dx * scale, y: rect.cy + dy * scale };
}

/**
 * A cubic bezier that always bows in the direction of travel.
 *
 * The sign terms are the fix for right-to-left arrows: without them the
 * control points push backwards and the curve loops through the source card.
 */
function curve(a, b) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const bow = Math.min(180, Math.max(45, Math.hypot(dx, dy) * 0.4));
  const sx = Math.sign(dx) || 1;
  const sy = Math.sign(dy) || 1;
  const [c1, c2] = Math.abs(dx) >= Math.abs(dy)
    ? [{ x: a.x + bow * sx, y: a.y }, { x: b.x - bow * sx, y: b.y }]
    : [{ x: a.x, y: a.y + bow * sy }, { x: b.x, y: b.y - bow * sy }];
  return `M ${a.x} ${a.y} C ${c1.x} ${c1.y}, ${c2.x} ${c2.y}, ${b.x} ${b.y}`;
}

export function renderArrows() {
  if (!pathsGroup) return;
  pathsGroup.replaceChildren();
  state.arrows = state.arrows.filter(a => cardById(a.from) && cardById(a.to));

  for (const arrow of state.arrows) {
    const from = rectOf(cardById(arrow.from));
    const to = rectOf(cardById(arrow.to));
    const a = edgePoint(from, to.cx, to.cy);
    const b = edgePoint(to, from.cx, from.cy);
    const d = curve(a, b);
    const selected = state.selectedArrow === arrow.id;

    const group = document.createElementNS(SVG_NS, 'g');
    if (selected) group.setAttribute('class', 'selected');

    // A wide invisible path makes a 2px line comfortably clickable.
    const hit = document.createElementNS(SVG_NS, 'path');
    hit.setAttribute('d', d);
    hit.setAttribute('class', 'hit');

    const line = document.createElementNS(SVG_NS, 'path');
    line.setAttribute('d', d);
    line.setAttribute('class', `link${selected ? ' selected' : ''}`);

    const pick = ev => {
      ev.stopPropagation();
      state.selectedArrow = selected ? null : arrow.id;
      renderArrows();
    };
    hit.addEventListener('pointerdown', ev => ev.stopPropagation());
    hit.addEventListener('click', pick);
    hit.addEventListener('dblclick', ev => { ev.stopPropagation(); relabel(arrow); });

    group.append(hit, line);

    if (arrow.label) {
      // Midpoint of the cubic at t = 0.5 keeps the chip on the line.
      const mid = { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
      const width = arrow.label.length * 6 + 12;
      const chip = document.createElementNS(SVG_NS, 'rect');
      chip.setAttribute('class', 'label-bg');
      chip.setAttribute('x', String(mid.x - width / 2));
      chip.setAttribute('y', String(mid.y - 9));
      chip.setAttribute('width', String(width));
      chip.setAttribute('height', '18');
      chip.setAttribute('rx', '4');

      const text = document.createElementNS(SVG_NS, 'text');
      text.setAttribute('class', 'label-tx');
      text.setAttribute('x', String(mid.x));
      text.setAttribute('y', String(mid.y + 4));
      text.setAttribute('text-anchor', 'middle');
      text.textContent = arrow.label;

      const hitLabel = document.createElementNS(SVG_NS, 'rect');
      hitLabel.setAttribute('x', String(mid.x - width / 2));
      hitLabel.setAttribute('y', String(mid.y - 9));
      hitLabel.setAttribute('width', String(width));
      hitLabel.setAttribute('height', '18');
      hitLabel.setAttribute('fill', 'transparent');
      hitLabel.style.pointerEvents = 'all';
      hitLabel.style.cursor = 'pointer';
      hitLabel.addEventListener('pointerdown', ev => ev.stopPropagation());
      hitLabel.addEventListener('click', pick);
      hitLabel.addEventListener('dblclick', ev => { ev.stopPropagation(); relabel(arrow); });

      group.append(chip, text, hitLabel);
    }

    pathsGroup.appendChild(group);
  }
}

function relabel(arrow) {
  const next = window.prompt('Arrow label (empty to clear):', arrow.label || '');
  if (next === null) return;
  arrow.label = next.trim().slice(0, 40);
  renderArrows();
  onChange();
}

/** Drags a new link from a card port; the card under the pointer is the target. */
function startLink(sourceCard, ev) {
  const temp = document.createElementNS(SVG_NS, 'path');
  temp.setAttribute('class', 'link temp');
  pathsGroup.appendChild(temp);
  document.body.classList.add('dragging');

  let hovered = null;

  const move = e => {
    const p = toWorld(e.clientX, e.clientY);
    temp.setAttribute('d', curve(edgePoint(rectOf(sourceCard), p.x, p.y), p));

    const overEl = document.elementFromPoint(e.clientX, e.clientY)?.closest('.card');
    const over = overEl ? cardById(Number(overEl.dataset.id)) : null;
    const next = over && over !== sourceCard ? over : null;
    if (next !== hovered) {
      hovered?.el.classList.remove('link-target');
      hovered = next;
      hovered?.el.classList.add('link-target');
    }
  };

  const up = e => {
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    document.body.classList.remove('dragging');
    temp.remove();
    hovered?.el.classList.remove('link-target');

    const overEl = document.elementFromPoint(e.clientX, e.clientY)?.closest('.card');
    const target = overEl ? cardById(Number(overEl.dataset.id)) : null;
    if (!target || target === sourceCard) return;

    if (state.arrows.some(a => a.from === sourceCard.id && a.to === target.id)) {
      toast('those cards are already linked');
      return;
    }
    state.arrows.push({ id: nextArrowId++, from: sourceCard.id, to: target.id, label: '' });
    renderArrows();
    onChange();
  };

  window.addEventListener('pointermove', move);
  window.addEventListener('pointerup', up);
}

/** Screen coordinates to world coordinates. */
export function toWorld(clientX, clientY) {
  const rect = document.getElementById('canvas').getBoundingClientRect();
  return {
    x: (clientX - rect.left - state.pan.x) / state.scale,
    y: (clientY - rect.top - state.pan.y) / state.scale,
  };
}

/** Adds one arrow between two cards, ignoring duplicates. */
export function addArrow(from, to, label = '') {
  if (!from || !to || from === to) return false;
  if (state.arrows.some(a => a.from === from.id && a.to === to.id)) return false;
  state.arrows.push({ id: nextArrowId++, from: from.id, to: to.id, label });
  renderArrows();
  onChange();
  return true;
}

/** Restores arrows from a saved layout, keeping ids unique afterwards. */
export function loadArrows(saved) {
  state.arrows = saved || [];
  nextArrowId = state.arrows.reduce((m, a) => Math.max(m, a.id), 0) + 1;
}

/** Draws arrows between every pair of open cards that import one another. */
export function linkImports(edges) {
  let added = 0;
  for (const [fromPath, toPath] of edges) {
    const from = state.cards.find(c => c.path === fromPath);
    const to = state.cards.find(c => c.path === toPath);
    if (!from || !to || from === to) continue;
    if (state.arrows.some(a => a.from === from.id && a.to === to.id)) continue;
    state.arrows.push({ id: nextArrowId++, from: from.id, to: to.id, label: '', auto: true });
    added++;
  }
  renderArrows();
  onChange();
  return added;
}

