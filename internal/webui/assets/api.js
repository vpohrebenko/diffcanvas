// Thin client for the local server. Every call carries the per-run token in a
// header; the page itself was loaded with the token in the query string.

const token = document.querySelector('meta[name="dc-token"]').content;

async function get(path, params = {}) {
  const url = new URL(path, location.origin);
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null) url.searchParams.set(k, v);
  }
  const res = await fetch(url, { headers: { 'X-Diffcanvas-Token': token } });
  const body = await res.json().catch(() => ({ error: `HTTP ${res.status}` }));
  if (!res.ok || body.error) throw new Error(body.error || `HTTP ${res.status}`);
  return body;
}

export const api = {
  meta: () => get('/api/meta'),
  changes: () => get('/api/changes'),
  file: (path, context) => get('/api/file', { path, context }),
  tree: () => get('/api/tree'),
  grep: (q, ignoreCase) => get('/api/grep', { q, i: ignoreCase ? '1' : '0' }),

  /** Import edges among the given paths; repeated ?p= params. */
  async imports(paths) {
    const url = new URL('/api/imports', location.origin);
    for (const p of paths) url.searchParams.append('p', p);
    const res = await fetch(url, { headers: { 'X-Diffcanvas-Token': token } });
    const body = await res.json().catch(() => ({ error: `HTTP ${res.status}` }));
    if (!res.ok || body.error) throw new Error(body.error || `HTTP ${res.status}`);
    return body;
  },
  def: (name, qual, from, line) => get('/api/def', { name, qual, from, line }),
  layout: () => get('/api/layout'),

  async saveLayout(layout) {
    await fetch('/api/layout', {
      method: 'PUT',
      headers: {
        'X-Diffcanvas-Token': token,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify(layout),
    });
  },
};
