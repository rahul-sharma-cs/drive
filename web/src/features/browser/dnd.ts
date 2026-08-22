/**
 * Dragging rows between folders.
 *
 * An internal drag carries its own MIME type and a JSON array of node ids. The
 * type is what separates a row on its way into another folder from files being
 * dragged in from the desktop, which carry `Files` — the drop zone gates on
 * that, and these three functions are the other half of the same agreement.
 *
 * `types` is the one part of a `DataTransfer` readable during `dragover` (the
 * payload is not), which is why the "is this ours" question is answered on it
 * and the ids are only read on `drop`.
 */

/** The MIME type an internal drag carries. Files dragged in carry `Files`. */
export const NODE_DRAG_TYPE = 'application/x-drive-node'

/** Puts `ids` on a drag that has just started. */
export function setDragPayload(dt: DataTransfer, ids: readonly string[]): void {
  dt.effectAllowed = 'move'
  dt.setData(NODE_DRAG_TYPE, JSON.stringify(ids))
}

/** The ids riding on an internal drag, or null if this is not one of ours. */
export function dragPayload(dt: DataTransfer | null): string[] | null {
  const raw = dt?.getData(NODE_DRAG_TYPE)
  if (!raw) return null
  try {
    const ids: unknown = JSON.parse(raw)
    return Array.isArray(ids) && ids.length > 0 ? (ids as string[]) : null
  } catch {
    return null
  }
}

/** True when a dragover carries our own payload — readable by type only. */
export function isNodeDrag(dt: DataTransfer | null): boolean {
  return dt !== null && Array.from(dt.types).includes(NODE_DRAG_TYPE)
}
