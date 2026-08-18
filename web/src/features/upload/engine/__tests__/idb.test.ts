/**
 * Record cache: whole-record writes at most ~1 Hz. Writing per progress tick
 * means concurrent read-modify-write cycles, which silently wipe fields.
 */

import { describe, expect, it } from 'vitest'
import { RecordCache, type UploadRecord } from '../idb'
import { FakeClock, FakeStorage } from './harness.test'

const record = (over: Partial<UploadRecord> = {}): UploadRecord => ({
  upload_id: 'up-1',
  fingerprint: 'fp',
  parent_id: 'parent-1',
  file_name: 'a.bin',
  file_size: 3000,
  part_size: 1000,
  parts_total: 3,
  confirmed_parts: [],
  status: 'active',
  session_expires_at: '2099-01-01T00:00:00Z',
  updated_at: 0,
  ...over,
})

describe('RecordCache', () => {
  it('coalesces a burst of saves into one leading and one trailing write', async () => {
    const clock = new FakeClock()
    const storage = new FakeStorage()
    const cache = new RecordCache(storage, clock, 1000)

    for (let i = 1; i <= 50; i++) cache.save(record({ confirmed_parts: [i] }))
    await clock.runUntil(() => storage.writes.length >= 2)

    expect(storage.writes.length).toBe(2)
    // The trailing write carries the LATEST record, whole — no lost fields.
    const last = storage.writes[storage.writes.length - 1]
    expect(last.confirmed_parts).toEqual([50])
    expect(last.file_name).toBe('a.bin')
    expect(last.parts_total).toBe(3)
  })

  it('writes again once the interval has elapsed', async () => {
    const clock = new FakeClock()
    const storage = new FakeStorage()
    const cache = new RecordCache(storage, clock, 1000)

    cache.save(record({ confirmed_parts: [1] }))
    await clock.runUntil(() => storage.writes.length === 1)
    await clock.advance(5000)
    cache.save(record({ confirmed_parts: [2] }))
    await clock.runUntil(() => storage.writes.length === 2)
    expect(storage.writes[1].confirmed_parts).toEqual([2])
  })

  it('finds a record by (fingerprint, parent_id) — two folders keep two records', async () => {
    const clock = new FakeClock()
    const storage = new FakeStorage()
    await storage.put(record({ upload_id: 'up-a', parent_id: 'folder-a' }))
    await storage.put(record({ upload_id: 'up-b', parent_id: 'folder-b' }))
    const cache = new RecordCache(storage, clock, 1000)

    expect((await cache.find('fp', 'folder-a'))?.upload_id).toBe('up-a')
    expect((await cache.find('fp', 'folder-b'))?.upload_id).toBe('up-b')
    expect(await cache.find('fp', 'folder-c')).toBeNull()
  })

  it('flush persists immediately and remove clears both layers', async () => {
    const clock = new FakeClock()
    const storage = new FakeStorage()
    const cache = new RecordCache(storage, clock, 1000)
    cache.save(record())
    await cache.flush()
    expect(storage.records.has('up-1')).toBe(true)
    await cache.remove('up-1')
    expect(storage.records.has('up-1')).toBe(false)
    expect(cache.get('up-1')).toBeUndefined()
  })
})
