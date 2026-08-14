// The changed-file tree.
//
// A real tree rather than a flat list: on a 50-file change the directory
// structure is most of the information about what the change actually touches.

import { state, el, DRAG_TYPE } from './store.js';

const collapsed = new Set();

/** Builds a nested node tree from flat paths. */
function buildTree(entries) {
  const root = { name: '', path: '', isDir: true, children: new Map(), adds: 0, dels: 0, files: 0 };

  for (const entry of entries) {
    const parts = entry.path.split('/');
    let node = root;
    for (let i = 0; i < parts.length; i++) {
      const isLeaf = i === parts.length - 1;
      const name = parts[i];
      if (!node.children.has(name)) {
        node.children.set(name, {
          name,
          path: parts.slice(0, i + 1).join('/'),
          isDir: !isLeaf,
          children: new Map(),
          adds: 0, dels: 0, files: 0,
          change: null,
        });
      }
      node = node.children.get(name);
      if (isLeaf) {
        node.isDir = false;
        node.change = entry.change || null;
      }
    }
  }
  tally(root);
  return compress(root);
}

/** Rolls per-file counts up into their directories. */
function tally(node) {
  if (!node.isDir) {
    node.adds = node.change?.adds || 0;
    node.dels = node.change?.dels || 0;
    node.files = 1;
    return;
  }
  node.adds = node.dels = node.files = 0;
  for (const child of node.children.values()) {
    tally(child);
    node.adds += child.adds;
    node.dels += child.dels;
    node.files += child.files;
  }
}

/**
 * Collapses chains of single-child directories into one row, so a Java-style
 * layout reads as `src/main/java/com/foo/` instead of six nested rows.
 */
function compress(node) {
  for (const [key, child] of node.children) {
    let merged = compress(child);
    while (merged.isDir && merged.children.size === 1) {
      const only = merged.children.values().next().value;
      if (!only.isDir) break;
      only.name = `${merged.name}/${only.name}`;
      merged = only;
    }
    node.children.set(key, merged);
  }
  return node;
}

function sortedChildren(node) {
  return [...node.children.values()].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
}

/**
 * Renders a tree into a container.
 *
 * onOpen(path, options) is called when a file row is activated; the same path
 * is also made draggable so it can be dropped at a chosen spot on the canvas.
 */
export function renderTree(container, entries, filterText, onOpen) {
  const needle = filterText.trim().toLowerCase();
  const filtered = needle
    ? entries.filter(e => e.path.toLowerCase().includes(needle))
    : entries;

  container.replaceChildren();
  if (!filtered.length) {
    container.appendChild(el('div', 'hint-text', needle ? 'nothing matches' : 'no files'));
    return;
  }

  const tree = buildTree(filtered);
  const frag = document.createDocumentFragment();
  // When filtering, expand everything so matches are visible immediately.
  const forceOpen = needle.length > 0;
  for (const child of sortedChildren(tree)) {
    renderNode(frag, child, 0, forceOpen, onOpen);
  }
  container.appendChild(frag);
}

function renderNode(parent, node, depth, forceOpen, onOpen) {
  const isOpen = forceOpen || !collapsed.has(node.path);
  const row = el('div', node.isDir ? 'node dir' : 'node file');
  row.style.paddingLeft = `${4 + depth * 11}px`;

  row.appendChild(el('span', 'twist', node.isDir ? (isOpen ? '▾' : '▸') : ''));

  const name = el('span', 'node-name');
  if (node.isDir) {
    name.textContent = `${node.name}/`;
  } else {
    name.textContent = node.name;
    if (node.change?.oldPath) {
      name.appendChild(el('span', 'dim', `  ← ${node.change.oldPath}`));
      name.title = `renamed from ${node.change.oldPath}`;
    }
  }
  row.appendChild(name);

  if (!node.isDir && node.change) {
    const dot = el('span', `dot st-${node.change.status}`);
    dot.title = node.change.status;
    row.appendChild(dot);
  }

  if (node.adds || node.dels) {
    row.appendChild(densityBar(node.adds, node.dels));
    const counts = el('span', 'counts');
    counts.append(el('span', 'a', `+${node.adds}`), document.createTextNode(' '), el('span', 'd', `−${node.dels}`));
    row.appendChild(counts);
  }

  if (node.isDir) {
    row.title = `${node.files} file${node.files === 1 ? '' : 's'}`;
    row.addEventListener('click', () => {
      if (collapsed.has(node.path)) collapsed.delete(node.path);
      else collapsed.add(node.path);
      window.dispatchEvent(new CustomEvent('dc:tree-refresh'));
    });
  } else {
    row.dataset.path = node.path;
    if (state.viewed.has(node.path)) row.classList.add('viewed');
    if (state.cards.some(c => c.path === node.path)) row.classList.add('open-on-canvas');
    row.title = `${node.path}\nclick to open · shift-click to mark reviewed`;

    row.addEventListener('click', ev => {
      if (ev.shiftKey) {
        if (state.viewed.has(node.path)) state.viewed.delete(node.path);
        else state.viewed.add(node.path);
        row.classList.toggle('viewed');
        return;
      }
      onOpen(node.path, {});
    });

    row.draggable = true;
    row.addEventListener('dragstart', ev => {
      ev.dataTransfer.effectAllowed = 'copy';
      ev.dataTransfer.setData(DRAG_TYPE, node.path);
    });
  }

  parent.appendChild(row);

  if (node.isDir && isOpen) {
    for (const child of sortedChildren(node)) {
      renderNode(parent, child, depth + 1, forceOpen, onOpen);
    }
  }
}

function densityBar(adds, dels) {
  const bar = el('div', 'dbar');
  const total = adds + dels;
  if (!total) return bar;
  const a = el('i', 'a');
  const d = el('i', 'd');
  a.style.width = `${(adds / total) * 100}%`;
  d.style.width = `${(dels / total) * 100}%`;
  bar.append(a, d);
  bar.title = `+${adds} −${dels}`;
  return bar;
}
