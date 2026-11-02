/** Thumbs up/down on a run or on one routing decision, with an optional comment. */
import { useCallback, useEffect, useState } from 'react'
import { getRunFeedback, submitRunFeedback } from '../api/sse'
import { Icon, ICON } from '../lib/icons'

export interface FeedbackTarget {
  namespace: string
  runName: string
}

export interface RunFeedback {
  /** The caller's rating by decision ID; the run-level rating is under ''. */
  ratings: Record<string, 1 | -1>
  rate: (decisionId: string, rating: 1 | -1, comment?: string) => Promise<void>
}

/** Loads the caller's ratings of a run once. Returns null when there is no run to
 *  rate or the operator has no PostgreSQL store, so callers render no thumbs. */
export function useRunFeedback(target?: FeedbackTarget): RunFeedback | null {
  const [ratings, setRatings] = useState<Record<string, 1 | -1> | null>(null)
  const ns = target?.namespace
  const run = target?.runName

  useEffect(() => {
    if (!ns || !run) return
    let live = true
    getRunFeedback(run, ns)
      .then((list) => {
        if (!live) return
        setRatings(Object.fromEntries(list.map((f) => [f.decisionId ?? '', f.rating])))
      })
      .catch(() => live && setRatings(null))
    return () => {
      live = false
    }
  }, [ns, run])

  const rate = useCallback(
    async (decisionId: string, rating: 1 | -1, comment?: string) => {
      if (!ns || !run) return
      const before = ratings?.[decisionId]
      setRatings((r) => ({ ...r, [decisionId]: rating }))
      try {
        await submitRunFeedback(run, ns, { decisionId: decisionId || undefined, rating, comment: comment || undefined })
      } catch (e) {
        // Put the thumb back where it was: the rating did not reach the server.
        setRatings((r) => {
          const next = { ...r }
          if (before === undefined) delete next[decisionId]
          else next[decisionId] = before
          return next
        })
        throw e
      }
    },
    [ns, run, ratings],
  )

  return ratings && target ? { ratings, rate } : null
}

interface Props {
  feedback: RunFeedback
  /** The decision to rate; leave out to rate the whole run. */
  decisionId?: string
  label: string
}

export function FeedbackThumbs({ feedback, decisionId = '', label }: Props) {
  const current = feedback.ratings[decisionId]
  const [error, setError] = useState(false)
  const [commenting, setCommenting] = useState(false)
  const [comment, setComment] = useState('')

  const rate = (rating: 1 | -1, text?: string) => {
    setError(false)
    feedback.rate(decisionId, rating, text).then(
      () => setCommenting(text === undefined),
      () => setError(true),
    )
  }

  const btn = (rating: 1 | -1, icon: typeof ICON.thumbsUp, name: string) => (
    <button
      type="button"
      aria-label={`${name}: ${label}`}
      aria-pressed={current === rating}
      title={name}
      onClick={() => rate(rating)}
      style={{
        background: 'none',
        border: 'none',
        padding: 2,
        cursor: 'pointer',
        color: current === rating ? 'var(--ds-accent)' : 'var(--ds-text-secondary)',
        opacity: current === rating ? 1 : 0.6,
      }}
    >
      <Icon icon={icon} size={13} />
    </button>
  )

  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, marginLeft: 6 }}>
      {btn(1, ICON.thumbsUp, 'Good choice')}
      {btn(-1, ICON.thumbsDown, 'Poor choice')}
      {error && <span role="alert" style={{ color: 'var(--ds-error)', fontSize: 11 }}>not saved</span>}
      {commenting && current !== undefined && (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            rate(current, comment.trim())
            setComment('')
          }}
          style={{ display: 'inline-flex', gap: 4 }}
        >
          <input
            aria-label={`Comment on ${label}`}
            value={comment}
            maxLength={500}
            placeholder="Add a comment"
            onChange={(e) => setComment(e.target.value)}
            style={{ fontSize: 11, padding: '1px 4px' }}
          />
          <button type="submit" style={{ fontSize: 11 }} disabled={!comment.trim()}>
            Send
          </button>
        </form>
      )}
    </span>
  )
}
