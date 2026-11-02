/** Hero output display card with copy support and markdown rendering. */
import { useState } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { DESIGN, ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'
import { FeedbackThumbs, useRunFeedback, type FeedbackTarget } from './FeedbackThumbs'

interface Props {
  output: string
  streaming?: boolean
  /** If true, render output as markdown. */
  markdown?: boolean
  /** The run that produced the output; with it, the whole run can be rated. */
  feedbackRun?: FeedbackTarget
}

export function OutputCard({ output, streaming, markdown, feedbackRun }: Props) {
  const feedback = useRunFeedback(streaming ? undefined : feedbackRun)
  const [copied, setCopied] = useState(false)
  const [open, setOpen] = useState(true)

  const copy = () => {
    navigator.clipboard.writeText(output).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div style={s.card}>
      <button type="button" style={s.header} onClick={() => setOpen((o) => !o)}
           aria-expanded={open} aria-controls="output-body"
      >
        <div style={s.title}>
          {streaming ? (
            <>
              <PulseDot />
              <span>Streaming output</span>
            </>
          ) : (
            <>
              <Icon icon={ICON.output} size={14} strokeWidth={1.5} />
              <span>AI Output</span>
            </>
          )}
        </div>
        <div style={s.actions}>
          <button type="button" style={s.iconBtn} onClick={(e) => { e.stopPropagation(); copy() }} aria-label="Copy output" title="Copy output">
            {copied ? <Icon icon={ICON.save} size={14} /> : <Icon icon={ICON.copy} size={14} />}
          </button>
          <Icon icon={ICON.chevronRight} size={11} style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : undefined }} />
        </div>
      </button>
      {open && (
        <div id="output-body" style={{ ...s.body, color: streaming ? 'var(--ds-text-secondary)' : 'var(--ds-text-primary)' }}>
          {markdown ? (
            <div style={s.mdWrap} className="ao-md">
              <Markdown remarkPlugins={[remarkGfm]}>{output}</Markdown>
            </div>
          ) : (
            <span>
              {output}
              {streaming && <span style={s.cursor}>▌</span>}
            </span>
          )}
        </div>
      )}
      {feedback && (
        <div style={{ padding: '6px 16px', borderTop: '1px solid var(--ds-border)', fontSize: 12, color: 'var(--ds-text-secondary)' }}>
          Was this run's model choice right?
          <FeedbackThumbs feedback={feedback} label="this run" />
        </div>
      )}
    </div>
  )
}

function PulseDot() {
  return (
    <span style={{
      display: 'inline-block',
      width: 7,
      height: 7,
      borderRadius: '50%',
      background: '#3b82f6',
      animation: 'aoPulse 1.5s ease infinite',
    }} />
  )
}

const s: Record<string, React.CSSProperties> = {
  card: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: DESIGN.radii.xl,
    display: 'flex',
    flexDirection: 'column',
    maxHeight: '50vh',
    boxShadow: 'var(--ds-card-shadow)',
    transitionProperty: 'box-shadow, border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  header: {
    padding: '12px 16px',
    borderBottom: `1px solid var(--ds-border)`,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    cursor: 'pointer',
    userSelect: 'none',
    background: 'transparent',
    border: 'none',
    color: 'inherit',
    font: 'inherit',
    textAlign: 'left',
    transitionProperty: 'border-color, background-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  title: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.08em',
    color: 'var(--ds-text-secondary)',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  actions: { display: 'flex', gap: 6, alignItems: 'center' },
  chevron: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    transitionProperty: 'transform',
    transitionDuration: '0.2s',
    transitionTimingFunction: 'ease',
  },
  iconBtn: {
    width: 28,
    height: 28,
    borderRadius: DESIGN.radii.sm,
    border: '1px solid var(--ds-border)',
    background: 'transparent',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: 13,
    transitionProperty: 'background-color, color, border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  body: {
    padding: 20,
    fontSize: 14,
    lineHeight: 1.7,
    fontFamily: 'system-ui, sans-serif',
    whiteSpace: 'pre-wrap',
    minHeight: 80,
    flex: 1,
    overflowY: 'auto',
    wordBreak: 'break-word',
    fontVariantNumeric: 'tabular-nums',
  },
  mdWrap: {
    color: 'var(--ds-text-primary)',
    fontSize: 14,
    lineHeight: 1.7,
    wordBreak: 'break-word',
  },
  // Opacity-only cursor blink — no scale transform (better-ui §16)
  cursor: {
    display: 'inline-block',
    animation: 'aoPulse 1s infinite',
    willChange: 'opacity',
  },
}
