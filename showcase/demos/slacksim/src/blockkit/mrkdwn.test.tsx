import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import { Mrkdwn, type MrkdwnContext } from './mrkdwn'

const ctx: MrkdwnContext = {
  resolveUser: (id) => ({ U1: 'jordan', U2: 'reviewbot' }[id] ?? id),
  resolveChannel: (id) => ({ C1: 'general', C2: 'alerts-prod' }[id] ?? id),
}

function renderMrkdwn(text: string) {
  return render(<Mrkdwn text={text} ctx={ctx} />)
}

describe('mrkdwn inline styles', () => {
  it('renders *bold* as a <strong>', () => {
    const { container } = renderMrkdwn('a *bold* word')
    const strong = container.querySelector('strong')
    expect(strong).not.toBeNull()
    expect(strong!.textContent).toBe('bold')
  })

  it('renders _italic_ as an <em>', () => {
    const { container } = renderMrkdwn('an _italic_ word')
    expect(container.querySelector('em')?.textContent).toBe('italic')
  })

  it('renders ~strike~ as a <del>', () => {
    const { container } = renderMrkdwn('a ~struck~ word')
    expect(container.querySelector('del')?.textContent).toBe('struck')
  })

  it('renders `inline code` as a <code>, without parsing markup inside', () => {
    const { container } = renderMrkdwn('call `do *not* bold` here')
    const code = container.querySelector('code')
    expect(code?.textContent).toBe('do *not* bold')
    expect(container.querySelector('strong')).toBeNull()
  })

  it('renders a triple-backtick block as a <pre>', () => {
    const { container } = renderMrkdwn('```\nline one\nline two\n```')
    const pre = container.querySelector('pre')
    expect(pre).not.toBeNull()
    expect(pre!.textContent).toContain('line one')
    expect(pre!.textContent).toContain('line two')
  })
})

describe('mrkdwn links and mentions', () => {
  it('renders <url|label> as an anchor with the label', () => {
    const { container } = renderMrkdwn('see <https://example.com/x|the docs> now')
    const a = container.querySelector('a')
    expect(a?.getAttribute('href')).toBe('https://example.com/x')
    expect(a?.textContent).toBe('the docs')
  })

  it('renders a bare <url> as an anchor showing the url', () => {
    const { container } = renderMrkdwn('go to <https://example.com>')
    const a = container.querySelector('a')
    expect(a?.getAttribute('href')).toBe('https://example.com')
    expect(a?.textContent).toBe('https://example.com')
  })

  it('resolves <@U1> to a @username mention pill', () => {
    renderMrkdwn('cc <@U1> please')
    expect(screen.getByText('@jordan')).toBeInTheDocument()
  })

  it('resolves <#C2|alerts-prod> to a #channel mention', () => {
    renderMrkdwn('posted in <#C2|alerts-prod>')
    expect(screen.getByText('#alerts-prod')).toBeInTheDocument()
  })

  it('resolves <#C1> (no label) via the channel resolver', () => {
    renderMrkdwn('see <#C1>')
    expect(screen.getByText('#general')).toBeInTheDocument()
  })

  it('renders <!here> as a @here broadcast pill', () => {
    renderMrkdwn('heads up <!here>')
    expect(screen.getByText('@here')).toBeInTheDocument()
  })
})

describe('mrkdwn emoji and whitespace', () => {
  it('maps a known :emoji: shortcode to its unicode', () => {
    renderMrkdwn('ship it :rocket:')
    expect(screen.getByText(/🚀/)).toBeInTheDocument()
  })

  it('leaves an unknown :shortcode: as literal text', () => {
    const { container } = renderMrkdwn('what :not_a_real_emoji_xyz:')
    expect(container.textContent).toContain(':not_a_real_emoji_xyz:')
  })

  it('turns newlines into <br> breaks', () => {
    const { container } = renderMrkdwn('line one\nline two')
    expect(container.querySelectorAll('br').length).toBe(1)
  })
})
