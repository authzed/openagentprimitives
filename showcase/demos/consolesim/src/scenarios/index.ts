import type { TermStory } from '../runtime/story'
import { oapInit } from './oap-init'
import { oapCheck } from './oap-check'

// The registry of built-in console stories. The capture engine and dev server
// resolve one by name via ?scenario=<name>.
export const STORIES: Record<string, () => TermStory> = {
  'oap-init': oapInit,
  'oap-check': oapCheck,
}

export const DEFAULT_SCENARIO = 'oap-init'

export function loadStory(name: string | null): TermStory {
  const key = name && name in STORIES ? name : DEFAULT_SCENARIO
  return STORIES[key]()
}
