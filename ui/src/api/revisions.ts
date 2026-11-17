/**
 * Client for revision history of UI-owned resources. Every save, restore and
 * delete made through the UI API leaves a revision; these calls list, read and
 * restore them. Listing and reading need the viewer role, restoring the builder role.
 */
import { failure, type KubeObject } from './builder'
import { apiFetch } from './sse'

export type RevisionAction = 'create' | 'update' | 'restore' | 'delete'

export interface RevisionSummary {
  revision: number
  action: RevisionAction
  userId: string
  /** RFC 3339. */
  createdAt: string
  /** A short plain-language description of the change; may be empty. */
  summary?: string
  resourceVersion?: string
  /** The revision a restore copied, only on `restore`. */
  restoredFrom?: number
}

/** A revision with the stored object (no status, ownership or server-owned metadata). */
export interface RevisionDetail extends RevisionSummary {
  object: KubeObject
}

export interface RevisionPage {
  /** Newest first. */
  revisions: RevisionSummary[]
  /** Pass as `before` for the next (older) page; empty when there is none. */
  next: string
}

const enc = encodeURIComponent
const base = (kind: string, ns: string, name: string) => `/api/resources/${enc(kind)}/${enc(ns)}/${enc(name)}/revisions`

/** Lists revisions, newest first. Throws ApiError (501 when revision history needs PostgreSQL). */
export async function listRevisions(
  kind: string, namespace: string, name: string, opts: { limit?: number; before?: string } = {},
): Promise<RevisionPage> {
  const q = new URLSearchParams()
  if (opts.limit) q.set('limit', String(opts.limit))
  if (opts.before) q.set('before', opts.before)
  const qs = q.toString()
  const res = await apiFetch(`${base(kind, namespace, name)}${qs ? `?${qs}` : ''}`)
  if (!res.ok) throw await failure(res)
  const body = (await res.json()) as Partial<RevisionPage>
  return { revisions: body.revisions ?? [], next: body.next ?? '' }
}

export async function getRevision(kind: string, namespace: string, name: string, n: number): Promise<RevisionDetail> {
  const res = await apiFetch(`${base(kind, namespace, name)}/${n}`)
  if (!res.ok) throw await failure(res)
  return res.json()
}

/**
 * Makes revision `n` the current version again. `resourceVersion` is the live
 * version the caller last loaded: a stale one gives 409, as does a Git-owned object.
 * Returns the restored object.
 */
export async function restoreRevision(
  kind: string, namespace: string, name: string, n: number, resourceVersion: string,
): Promise<KubeObject> {
  const res = await apiFetch(`${base(kind, namespace, name)}/${n}/restore`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resourceVersion }),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}
