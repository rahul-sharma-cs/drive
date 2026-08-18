// @vitest-environment jsdom

import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { UploadSnapshot, UploadState } from '../../engine/types'
import type { UploadActions } from '../engineStore'
import { UploadManager } from '../UploadManager'

function item(over: Partial<UploadSnapshot> = {}): UploadSnapshot {
  return {
    id: 'u1',
    upload_id: 'srv-1',
    name: 'video.mov',
    original_name: 'video.mov',
    renamed: false,
    size: 4 * 1024 * 1024,
    parent_id: 'root-1',
    state: 'uploading',
    progress: 0.5,
    bytes_confirmed: 2 * 1024 * 1024,
    parts_total: 4,
    parts_confirmed: 2,
    speed_bps: 1024 * 1024,
    eta_seconds: 2,
    error_code: null,
    error: null,
    session_expires_at: null,
    node_id: null,
    verify_parts: null,
    ...over,
  }
}

function actions(): UploadActions {
  return {
    enqueue: vi.fn(),
    pause: vi.fn(),
    resume: vi.fn(),
    retry: vi.fn(),
    cancel: vi.fn(),
    reselect: vi.fn(),
    resolveConflict: vi.fn(),
    clearFinished: vi.fn(),
  } as unknown as UploadActions
}

describe('upload manager', () => {
  it('renders the part count and the progress the engine reports', () => {
    render(<UploadManager items={[item()]} actions={actions()} />)

    expect(screen.getByText(/2\/4 parts/)).toBeTruthy()
    expect(screen.getByRole('progressbar').getAttribute('aria-valuenow')).toBe('50')
  })

  it('distinguishes an offline pause from an unreachable server', () => {
    render(
      <UploadManager
        items={[item({ id: 'a', state: 'paused_offline' }), item({ id: 'b', state: 'paused_backend' })]}
        actions={actions()}
      />,
    )

    expect(screen.getByText('Paused — offline. Will resume automatically.')).toBeTruthy()
    expect(screen.getByText("Paused — can't reach the server. Will resume automatically.")).toBeTruthy()
    // Neither is the user's problem to fix, so neither offers Resume.
    expect(screen.queryByRole('button', { name: 'Resume' })).toBeNull()
  })

  it('offers pause while running and resume once paused', async () => {
    const a = actions()
    const { rerender } = render(<UploadManager items={[item()]} actions={a} />)
    await userEvent.click(screen.getByRole('button', { name: 'Pause' }))
    expect(a.pause).toHaveBeenCalledWith('u1')

    rerender(<UploadManager items={[item({ state: 'paused' })]} actions={a} />)
    await userEvent.click(screen.getByRole('button', { name: 'Resume' }))
    expect(a.resume).toHaveBeenCalledWith('u1')
  })

  it('asks for the file again when it changed on disk, and re-selects it', async () => {
    const a = actions()
    render(<UploadManager items={[item({ state: 'error_file_changed' })]} actions={a} />)

    expect(screen.getByText(/pick it again to restart/i)).toBeTruthy()
    const picked = new File(['fresh bytes'], 'video.mov')
    await userEvent.upload(screen.getByLabelText('Pick video.mov again'), picked)

    expect(a.reselect).toHaveBeenCalledWith('u1', picked)
  })

  it('badges a server-side auto-rename with both names', () => {
    render(
      <UploadManager
        items={[item({ state: 'done', progress: 1, name: 'video (1).mov', renamed: true })]}
        actions={actions()}
      />,
    )

    const badge = screen.getByText('renamed')
    expect(badge.getAttribute('title')).toContain('video (1).mov')
    expect(badge.getAttribute('title')).toContain('video.mov')
  })

  it('offers Try again on a failed upload and on an expired session', async () => {
    const a = actions()
    const states: UploadState[] = ['failed', 'session_expired']
    for (const state of states) {
      const { unmount } = render(<UploadManager items={[item({ state, error: 'nope' })]} actions={a} />)
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
      unmount()
    }
    expect(a.retry).toHaveBeenCalledTimes(2)
  })

  it('shows nothing at all when no upload has been started', () => {
    const { container } = render(<UploadManager items={[]} actions={actions()} />)
    expect(container.firstChild).toBeNull()
  })
})
