// Add ElevenLabs voiceover to the existing real-product Agent Builder capture.
// The source is the web UI clip already published in docs/public/media. Keeping
// the source intact makes the voice pass repeatable without starting a new
// workshop or installing another agent in a cluster.
// Usage: ELEVENLABS_API_KEY=... node engine/capture/narrate-builder.mjs [outBase]
import { mkdirSync } from 'node:fs'
import path from 'node:path'
import { narrate } from '../tts/narrate.mjs'
import { encodeClip } from '../tts/mux.mjs'

if (!process.env.ELEVENLABS_API_KEY) throw new Error('set ELEVENLABS_API_KEY to narrate the builder clip')

const source = path.join('docs', 'public', 'media', 'builder-first-agent.mp4')
const outBase = process.argv[2] || path.join('out', 'clips', 'builder-first-agent')
mkdirSync(path.dirname(outBase), { recursive: true })

// Starts track the visible stages of the existing 88-second web UI footage.
// Lines stay short enough to finish before the next stage begins.
const lines = [
  { id: 'choose', startMs: 1000, text: 'Meet the Agent Builder. Start a session and choose Agent Builder from the list.' },
  { id: 'describe', startMs: 10500, text: 'Describe your agent in plain language. Here, we ask for a friendly limerick bot with no tools.' },
  { id: 'questions', startMs: 20500, text: 'The builder restates the goal, records decisions, and asks about anything still unclear.' },
  { id: 'approve', startMs: 30500, text: 'Once you answer, it moves through tools and permissions. You approve a stage before it changes the draft.' },
  { id: 'build', startMs: 40500, text: 'It builds the agent and checks the draft until the platform reports it valid.' },
  { id: 'test', startMs: 50500, text: 'Next, test the new agent right here in the web UI, using a real chat session.' },
  { id: 'deliver', startMs: 60500, text: 'The builder reads the test result and saves a portable copy of exactly what you tried.' },
  { id: 'install', startMs: 70500, text: 'Installing is a separate decision. An admin chooses the namespace and approves the request.' },
  { id: 'finished', startMs: 80500, text: 'Now the new agent is installed.' },
]

const audio = await narrate(lines, path.join('out', 'audio', 'builder-first-agent'))
for (let i = 0; i < lines.length - 1; i++) {
  if (lines[i].startMs + audio[i].durationMs > lines[i + 1].startMs) {
    throw new Error(`voice line ${lines[i].id} overlaps ${lines[i + 1].id}; shorten the script or adjust its cue`)
  }
}
encodeClip(source, outBase, lines.map((line, i) => ({ path: audio[i].path, startMs: line.startMs })))
console.log(`wrote ${outBase}.webm, ${outBase}.mp4, ${outBase}.png`)
