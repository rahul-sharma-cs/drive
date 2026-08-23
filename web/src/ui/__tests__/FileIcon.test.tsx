// @vitest-environment jsdom

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { FILE_ICON_TABLE, FileIcon, fileCategory } from '../FileIcon'

const svgOf = (container: HTMLElement) => container.querySelector('svg')!

describe('fileCategory', () => {
  it('ignores mime on a folder', () => {
    expect(fileCategory('folder', 'anything.pdf', 'application/pdf')).toBe('folder')
  })

  it('lets mime win over a misleading extension', () => {
    expect(fileCategory('file', 'resume.txt', 'application/pdf')).toBe('pdf')
  })

  it('reads a type off the mime with no filename extension to help', () => {
    expect(fileCategory('file', 'scan', 'image/png')).toBe('image')
  })

  it('classifies a compound .tar.gz as an archive', () => {
    expect(fileCategory('file', 'backup.tar.gz', null)).toBe('archive')
  })

  it('falls back to generic for an unrecognised type', () => {
    expect(fileCategory('file', 'mystery.xyz123', null)).toBe('generic')
  })
})

describe('FileIcon', () => {
  it('draws application/pdf and a bare .pdf as the same glyph in the same hue, decoratively', () => {
    const byMime = render(<FileIcon kind="file" name="report" mime="application/pdf" />)
    const first = svgOf(byMime.container)
    expect(first.getAttribute('aria-hidden')).toBe('true')
    expect(screen.queryByRole('img')).toBeNull()
    byMime.unmount()

    const byExtension = render(<FileIcon kind="file" name="report.pdf" mime="" />)
    const second = svgOf(byExtension.container)

    // Same drawing, same colour, whichever of the two said so.
    expect(second.getAttribute('class')).toBe(first.getAttribute('class'))
    expect(first.getAttribute('class')).toContain('text-danger')
  })

  it('gives each type its own hue out of the product’s own tokens', () => {
    // The point of the tokens: a list of mixed types is one palette. A stock
    // shade slipping back into the table would show up here as a class that is
    // not one of ours.
    const hues = [
      ['scan.png', 'image/png', 'text-type-image'],
      ['clip.mp4', 'video/mp4', 'text-type-video'],
      ['take.mp3', 'audio/mpeg', 'text-type-audio'],
      ['backup.tar.gz', null, 'text-type-archive'],
      ['books.xlsx', null, 'text-type-sheet'],
      ['deal.docx', null, 'text-type-doc'],
      ['server.go', null, 'text-type-code'],
      ['notes.txt', 'text/plain', 'text-type-text'],
    ] as const

    for (const [name, mime, expected] of hues) {
      const { container, unmount } = render(<FileIcon kind="file" name={name} mime={mime} />)
      expect(svgOf(container).getAttribute('class')).toContain(expected)
      unmount()
    }
  })

  it('does not draw the PDF, the document and the text file with one glyph', () => {
    // All three used to be the same page of ruled lines, which left the hue
    // carrying the entire difference between three of the commonest things in
    // a folder — and two of the three hues are a blue and a grey. Whatever the
    // drawings are, they cannot be one drawing.
    const drawing = (name: string, mime: string) => {
      const { container, unmount } = render(<FileIcon kind="file" name={name} mime={mime} />)
      // The paths only. The colour class already differs and is not the point.
      const paths = svgOf(container).innerHTML
      unmount()
      return paths
    }

    const drawings = new Set([
      drawing('lease.pdf', 'application/pdf'),
      drawing('deal.docx', 'application/vnd.openxmlformats-officedocument.wordprocessingml.document'),
      drawing('notes.txt', 'text/plain'),
    ])
    expect(drawings.size).toBeGreaterThanOrEqual(2)
  })

  it('fills a folder and outlines a file, so the two never depend on colour alone', () => {
    const folder = render(<FileIcon kind="folder" name="Invoices" />)
    expect(svgOf(folder.container).getAttribute('fill')).toBe('currentColor')
    expect(svgOf(folder.container).getAttribute('class')).toContain('text-type-folder')
    folder.unmount()

    const file = render(<FileIcon kind="file" name="notes.txt" mime="text/plain" />)
    expect(svgOf(file.container).getAttribute('fill')).not.toBe('currentColor')
  })

  it('draws no lettering inside the glyph at any size', () => {
    // Three bold letters over the ruled lines of a small page is a smudge, not
    // a label — and the row already spells the extension out in the name. The
    // glyph and its hue carry the type on their own.
    for (const size of [20, 22, 24, 40]) {
      const { container, unmount } = render(
        <FileIcon kind="file" name="lease-signed.pdf" mime="application/pdf" size={size} />,
      )
      expect(container.textContent).toBe('')
      unmount()
    }
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

  it('never reaches outside the product’s own colour tokens', () => {
    for (const spec of FILE_ICON_TABLE) {
      expect(spec.colorClass).toMatch(/^text-(type-[a-z]+|danger|warn|ink-3)$/)
    }
  })
})
