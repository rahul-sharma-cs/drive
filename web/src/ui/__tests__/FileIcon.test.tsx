// @vitest-environment jsdom

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { FILE_ICON_TABLE, FileIcon, fileCategory } from '../FileIcon'

describe('fileCategory', () => {
  it('ignores mime on a folder', () => {
    expect(fileCategory('folder', 'anything.pdf', 'application/pdf')).toBe('folder')
  })

  it('lets mime win over a misleading extension', () => {
    expect(fileCategory('file', 'resume.txt', 'application/pdf')).toBe('pdf')
  })
})

describe('FileIcon', () => {
  it('renders application/pdf and a bare .pdf extension as the same "PDF" glyph, decoratively', () => {
    const byMime = render(<FileIcon kind="file" name="report" mime="application/pdf" />)
    expect(screen.getByText('PDF')).toBeTruthy()
    expect(byMime.container.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true')
    byMime.unmount()

    const byExtension = render(<FileIcon kind="file" name="report.pdf" mime="" />)
    expect(screen.getByText('PDF')).toBeTruthy()
    byExtension.unmount()
  })

  it('tags image/png "PNG" even without a matching filename extension', () => {
    render(<FileIcon kind="file" name="scan" mime="image/png" />)
    expect(screen.getByText('PNG')).toBeTruthy()
  })

  it('classifies and tags a compound .tar.gz as an archive', () => {
    expect(fileCategory('file', 'backup.tar.gz', null)).toBe('archive')
    render(<FileIcon kind="file" name="backup.tar.gz" mime={null} />)
    expect(screen.getByText('TGZ')).toBeTruthy()
  })

  it('falls back to a generic glyph with no accessible role for an unrecognised type', () => {
    expect(fileCategory('file', 'mystery.xyz123', null)).toBe('generic')
    render(<FileIcon kind="file" name="mystery.xyz123" mime={null} />)
    expect(screen.queryByRole('img')).toBeNull()
  })
})

describe('FILE_ICON_TABLE', () => {
  it('never lists the same extension under two categories', () => {
    const seen = new Set<string>()
    for (const spec of FILE_ICON_TABLE) {
      for (const ext of spec.extensions) {
        expect(seen.has(ext)).toBe(false)
        seen.add(ext)
      }
    }
  })
})
