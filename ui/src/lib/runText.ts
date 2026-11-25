/**
 * Turns a run's raw input and output into text a person can scan: lists show
 * what was asked, not the run's generated name or the channel's wrapper.
 */

export interface RunInput {
  /** What was asked. Empty when a list cut the input off before the message. */
  message: string
  /** Set when the run came from a channel such as Slack or email. */
  channel?: string
  sender?: string
  subject?: string
}

const CHANNEL_PREFIX = /^Message received through the (\S+) channel\.\n/
const MESSAGE_MARK = '\n--- message ---\n'
const ATTACHMENT_MARK = '\n--- attachment '

/**
 * Reads the format written by FormatInput in internal/inbound/pipeline.go.
 * Everything after the first message mark is the sender's own text, so labelled
 * fields are only read from before it. For display only: any input can imitate
 * this shape.
 */
export function parseRunInput(input: string): RunInput {
  const head = CHANNEL_PREFIX.exec(input)
  if (!head) return { message: input }
  const at = input.indexOf(MESSAGE_MARK)
  const labels = at < 0 ? input : input.slice(0, at)
  const body = at < 0 ? '' : input.slice(at + MESSAGE_MARK.length)
  const attachment = body.indexOf(ATTACHMENT_MARK)
  const out: RunInput = { message: attachment < 0 ? body : body.slice(0, attachment), channel: head[1] }
  const sender = /^Sender: (.*)$/m.exec(labels)?.[1]
  const subject = /^Subject: (.*)$/m.exec(labels)?.[1]
  if (sender) out.sender = sender
  if (subject) out.subject = subject
  return out
}

/** One line naming a run: the start of what was asked, else the run's name. */
export function runTitle(input: string | undefined, runName: string): string {
  const { message, subject } = parseRunInput(input ?? '')
  const line = (subject || message).split('\n').map((l) => l.trim()).find(Boolean)
  return line ? plainPreview(line) || runName : runName
}

/** Display name of a channel kind. */
export function channelLabel(kind: string): string {
  return kind.charAt(0).toUpperCase() + kind.slice(1)
}

/** Markdown reduced to a single line of plain text, for previews. */
export function plainPreview(markdown: string): string {
  return markdown
    .replace(/```[\s\S]*?(```|$)/g, ' ')
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/^\s*\|?[\s:|-]*-{3}[\s:|-]*$/gm, ' ')
    .replace(/^\s{0,3}(#{1,6}|>|[-*+]|\d+\.)\s+/gm, '')
    .replace(/\*\*|__|[*`]/g, '')
    .replace(/\|/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
}
