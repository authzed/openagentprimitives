import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import { Blocks } from './BlockKit'
import type { Block } from './types'
import type { MrkdwnContext } from './mrkdwn'
import approval from '../fixtures/blockkit/approval.json'
import plan from '../fixtures/blockkit/plan.json'
import message from '../fixtures/blockkit/message.json'

// These fixtures are the REAL Block Kit JSON OAP's slack kind emits, captured by
// `mage blocks:capture`. Rendering them proves the demo shows what OAP actually
// sends — and that the renderer keeps up with OAP's custom container/plan blocks.
const ctx: MrkdwnContext = { resolveUser: (id) => id, resolveChannel: (id) => id }

describe('rendering real captured OAP Block Kit', () => {
  it('renders the approval container with its title and Approve/Deny buttons', () => {
    render(<Blocks blocks={approval as Block[]} ctx={ctx} />)
    expect(screen.getByText(/Deploy .+ to production\?/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Deny' })).toBeInTheDocument()
  })

  it('renders the plan block as a task checklist', () => {
    const { container } = render(<Blocks blocks={plan as Block[]} ctx={ctx} />)
    expect(container.querySelectorAll('.sk-plan-tasks li').length).toBe(4)
    // The in-progress task carries its status modifier class.
    expect(container.querySelector('.sk-task--in_progress')).not.toBeNull()
  })

  it('renders the completion message with a Show settings button', () => {
    render(<Blocks blocks={message as Block[]} ctx={ctx} />)
    expect(screen.getByText(/Rollout complete/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Show settings' })).toBeInTheDocument()
  })
})
