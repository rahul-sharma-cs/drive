// @vitest-environment jsdom

import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { UploadSession } from '../../../../lib/api'
import type { UploadSnapshot } from '../../engine/types'
import { ConflictDialog } from '../ConflictDialog'
import { resumableSessions } from '../resumable'
import { ResumableUploads } from '../ResumableUploads'

function session(over: Partial<UploadSession> = {}): UploadSession {
  return {
    upload_id: 'srv-1',
    file_name: 'archive.zip',
    file_size: 400 * 1024 * 1024,
    part_size: 100 * 1024 * 1024,
    parts_total: 4,
    parent_id: 'folder-7',
    status: 'active',
    confirmed_parts: [1, 2],
    session_expires_at: '2026-08-18T00:00:00Z',
    ...over,
  }
}

function snapshot(over: Partial<UploadSnapshot> = {}): UploadSnapshot {
  return {
    id: 'u1',
    upload_id: null,
    name: 'archive.zip',
    original_name: 'archive.zip',
    renamed: false,
    size: 10,
    parent_id: 'folder-7',
    state: 'uploading',
    progress: 0,
    bytes_confirmed: 0,
    parts_total: 4,
    parts_confirmed: 0,
    speed_bps: null,
    eta_seconds: null,
    error_code: null,
    error: null,
    session_expires_at: null,
    node_id: null,
    verify_parts: null,
    ...over,
  }
}

describe('resumable sessions', () => {
  it('offers an interrupted session back', () => {
    expect(resumableSessions([], [session()])).toHaveLength(1)
  })

  it('drops a session an engine row is already driving', () => {
    // Otherwise the upload that was just resumed appears twice: once as a live
    // row and once as an invitation to start it again.
    expect(resumableSessions([snapshot({ upload_id: 'srv-1' })], [session()])).toEqual([])
  })

  it('ignores sessions that are no longer active', () => {
    const finished: UploadSession['status'][] = ['completing', 'done', 'aborted']
    for (const status of finished) {
      expect(resumableSessions([], [session({ status })])).toEqual([])
    }
  })
})

describe('resumable rows', () => {
  it('asks for the same file and hands it to the session’s folder', async () => {
    const onPick = vi.fn()
    render(<ResumableUploads sessions={[session()]} rootId="root-1" onPick={onPick} onDiscard={vi.fn()} />)

    expect(screen.getByText(/2 of 4 parts are already uploaded/i)).toBeTruthy()
    expect(screen.getByText(/pick the same file/i)).toBeTruthy()

    const file = new File(['bytes'], 'archive.zip')
    await userEvent.upload(screen.getByLabelText('Pick archive.zip to resume'), file)
    expect(onPick).toHaveBeenCalledWith(file, 'folder-7')
  })

  it('falls back to the root when the destination folder was purged', async () => {
    const onPick = vi.fn()
    render(
      <ResumableUploads sessions={[session({ parent_id: null })]} rootId="root-1" onPick={onPick} onDiscard={vi.fn()} />,
    )

    const file = new File(['bytes'], 'archive.zip')
    await userEvent.upload(screen.getByLabelText('Pick archive.zip to resume'), file)
    expect(onPick).toHaveBeenCalledWith(file, 'root-1')
  })
})

describe('conflict prompt', () => {
  const conflicted = [
    snapshot({ id: 'a', state: 'conflict', original_name: 'report.pdf' }),
    snapshot({ id: 'b', state: 'conflict', original_name: 'notes.txt' }),
    snapshot({ id: 'c', state: 'conflict', original_name: 'photo.jpg' }),
  ]

  it('prompts once for a bulk drop, not once per file', () => {
    render(<ConflictDialog conflicts={conflicted} onResolve={vi.fn()} onSkip={vi.fn()} />)

    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    expect(screen.getByText(/“report.pdf” is already here/)).toBeTruthy()
    expect(screen.getByText(/other 2 files too/i)).toBeTruthy()
  })

  it('resolves only the prompted upload by default', async () => {
    const onResolve = vi.fn()
    render(<ConflictDialog conflicts={conflicted} onResolve={onResolve} onSkip={vi.fn()} />)

    await userEvent.click(screen.getByRole('button', { name: 'Keep both' }))
    expect(onResolve).toHaveBeenCalledWith(['a'], 'rename')
  })

  it('applies to every conflicted upload when asked to', async () => {
    const onResolve = vi.fn()
    render(<ConflictDialog conflicts={conflicted} onResolve={onResolve} onSkip={vi.fn()} />)

    await userEvent.click(screen.getByRole('checkbox'))
    await userEvent.click(screen.getByRole('button', { name: 'Replace' }))
    expect(onResolve).toHaveBeenCalledWith(['a', 'b', 'c'], 'replace')
  })

  it('stays open until a choice is made', async () => {
    render(<ConflictDialog conflicts={conflicted} onResolve={vi.fn()} onSkip={vi.fn()} />)

    await userEvent.keyboard('{Escape}')
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
  })
})
