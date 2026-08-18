/**
 * The singleton store contract the React layer consumes:
 * `subscribe(cb)` + `getSnapshot()` shaped for useSyncExternalStore.
 */

import { describe, expect, it } from 'vitest'
import { uploadEngine } from '../engine'
import { makeFile, makeHarness } from './harness.test'

describe('useSyncExternalStore contract', () => {
  it('returns a cached snapshot whose identity only changes on real changes', async () => {
    const h = makeHarness()
    const first = h.engine.getSnapshot()
    expect(h.engine.getSnapshot()).toBe(first) // stable between changes

    await h.clock.runUntil(() => h.snap().state === 'done')
    const after = h.engine.getSnapshot()
    expect(after).not.toBe(first)
    expect(h.engine.getSnapshot()).toBe(after)
  })

  it('notifies subscribers and stops after unsubscribe', async () => {
    const h = makeHarness()
    let calls = 0
    const unsubscribe = h.engine.subscribe(() => {
      calls++
    })
    await h.clock.runUntil(() => h.snap().state === 'done')
    expect(calls).toBeGreaterThan(0)

    const seen = calls
    unsubscribe()
    h.engine.clearFinished()
    expect(calls).toBe(seen)
  })

  it('exposes one item per enqueued file and runs them one at a time', async () => {
    const h = makeHarness()
    const second = h.engine.enqueue(makeFile(1000, 'b.bin'), 'parent-1')

    const states = () => Object.fromEntries(h.engine.getSnapshot().items.map((i) => [i.id, i.state]))
    expect(h.engine.getSnapshot().items.length).toBe(2)
    expect(states()[second]).toBe('queued')

    await h.clock.runUntil(() => states()[second] === 'done')
    expect(states()[h.id]).toBe('done')
  })

  it('clearFinished drops completed rows', async () => {
    const h = makeHarness()
    await h.clock.runUntil(() => h.snap().state === 'done')
    h.engine.clearFinished()
    expect(h.engine.getSnapshot().items).toEqual([])
  })
})

describe('pausing one upload never freezes the others', () => {
  it('starts the next queued upload while the first is paused', async () => {
    // The hash workers are SHARED (one pair for the engine) and `suspend` sets a
    // module-global flag in the worker, so a pause that suspends them while
    // another upload needs them leaves that upload stuck in `preparing` with no
    // error and no recovery — the 150-file folder drop scenario.
    const h = makeHarness()
    const second = h.engine.enqueue(makeFile(1000, 'b.bin'), 'parent-2')
    const state = (id: string) => h.engine.getSnapshot().items.find((i) => i.id === id)!.state

    h.put.hold.add(1)
    await h.clock.runUntil(() => h.put.puts.length >= 1)

    h.engine.pause(h.id)
    expect(state(h.id)).toBe('paused')

    // maxActive is 1, so pausing the first admits the second — which must be
    // able to hash. (The suspend is deliberately skipped here: `pause()` let
    // `schedule()` start the second upload before the engine looked, so the
    // shared workers are still in use.)
    await h.clock.runUntil(() => state(second) === 'uploading')
    expect(h.md5.suspends).toBe(0)
  })

  it('suspends the shared workers when nothing else is running', async () => {
    const h = makeHarness()
    h.put.hold.add(1)
    await h.clock.runUntil(() => h.put.puts.length >= 1)

    h.engine.pause(h.id)
    expect(h.md5.suspends).toBe(1)
    expect(h.sha256.suspends).toBe(1)
  })
})

describe('pause on a queued upload', () => {
  it('is honoured — the engine never starts it behind the user’s back', async () => {
    const h = makeHarness()
    const second = h.engine.enqueue(makeFile(1000, 'b.bin'), 'parent-2')
    const state = (id: string) => h.engine.getSnapshot().items.find((i) => i.id === id)!.state
    expect(state(second)).toBe('queued')

    h.engine.pause(second)
    expect(state(second)).toBe('paused')

    await h.clock.runUntil(() => state(h.id) === 'done')
    await h.clock.settle()
    expect(state(second)).toBe('paused')

    h.engine.resume(second)
    await h.clock.runUntil(() => state(second) === 'done')
  })

  it('resuming re-enters the queue instead of exceeding maxActive', async () => {
    const h = makeHarness()
    const second = h.engine.enqueue(makeFile(1000, 'b.bin'), 'parent-2')
    const items = () => h.engine.getSnapshot().items
    const active = () =>
      items().filter((i) => i.state === 'preparing' || i.state === 'uploading' || i.state === 'completing')
        .length

    h.put.hold.add(1)
    await h.clock.runUntil(() => h.put.puts.length >= 1)
    h.engine.pause(h.id) // second one starts under the cap

    await h.clock.runUntil(() => items().find((i) => i.id === second)!.state === 'uploading')
    h.engine.resume(h.id)
    expect(active()).toBeLessThanOrEqual(1)

    h.put.hold.clear()
    await h.clock.runUntil(() => items().every((i) => i.state === 'done'))
  })
})

describe('reselect', () => {
  it('hands a re-picked File to the same row and re-fingerprints it', async () => {
    // `error_file_changed` routes into the re-select + fingerprint flow: a
    // browser cannot re-read a file after a reload or an on-disk change, so
    // the only way forward is the user picking the file again.
    const dead = makeFile(3000)
    const err = Object.assign(new Error('could not be read'), { name: 'NotReadableError' })
    Object.defineProperty(dead, 'slice', { value: () => ({ arrayBuffer: () => Promise.reject(err) }) })
    const h = makeHarness({ file: dead })
    h.md5.fail = err

    await h.clock.runUntil(() => h.snap().state === 'error_file_changed')
    const createsBefore = h.server.calls.filter((c) => c.startsWith('create')).length

    h.engine.reselect(h.id, makeFile(3000))

    await h.clock.runUntil(() => h.snap().state === 'done')
    // A fresh POST /uploads carried the recomputed fingerprint, and the row kept
    // its id — cancel + re-enqueue would have lost it.
    expect(h.server.calls.filter((c) => c.startsWith('create')).length).toBe(createsBefore + 1)
    expect(h.snap().id).toBe(h.id)
    expect(h.snap().error_code).toBeNull()
  })
})

describe('module-scope singleton', () => {
  it('imports in a node environment without touching any browser global', () => {
    // Constructing it must not read navigator/document/indexedDB — a React
    // route change must never unmount an upload, so it lives outside the tree.
    expect(uploadEngine.getSnapshot().items).toEqual([])
    expect(typeof uploadEngine.subscribe).toBe('function')
    expect(typeof uploadEngine.getSnapshot).toBe('function')
  })
})
