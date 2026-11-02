import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { FrontierChart } from './FrontierChart'

const points = [
  { threshold: 0.3, costUsd: 0.0021, loss: 0.1 },
  { threshold: 0.5, costUsd: 0.0034, loss: 0.1, current: true },
  { threshold: 0.7, costUsd: 0.0051, loss: 0.02 },
]

describe('FrontierChart', () => {
  it('labels the threshold in use and the one proposed, by name and not only by colour', () => {
    render(<FrontierChart points={points} proposed={0.3} />)
    expect(screen.getByText('in use 0.50')).toBeInTheDocument()
    expect(screen.getByText('proposed 0.30')).toBeInTheDocument()
    expect(screen.getByRole('img', { name: /3 thresholds/ })).toBeInTheDocument()
  })

  it('shows a point\'s numbers when it is hovered', () => {
    const { container } = render(<FrontierChart points={points} proposed={0.3} />)
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument()
    // Hover the first plotted mark (the threshold 0.30 point).
    const plotted = Array.from(container.querySelectorAll('svg g')).filter((g) => g.querySelector('circle[r="12"]'))
    expect(plotted).toHaveLength(3)
    fireEvent.mouseEnter(plotted[0])
    const tip = screen.getByRole('tooltip')
    expect(tip).toHaveTextContent('0.30 (proposed)')
    expect(tip).toHaveTextContent('$0.00210 per call')
    expect(tip).toHaveTextContent('10.0% routed to the losing model')
  })

  it('offers the same numbers as a table', () => {
    render(<FrontierChart points={points} proposed={0.3} />)
    fireEvent.click(screen.getByRole('button', { name: 'Show as table' }))
    const table = screen.getByRole('table', { name: /by threshold/ })
    expect(table).toHaveTextContent('0.50')
    expect(table).toHaveTextContent('in use')
    expect(table).toHaveTextContent('proposed')
    expect(table).toHaveTextContent('2.0%')
    fireEvent.click(screen.getByRole('button', { name: 'Show as chart' }))
    expect(screen.getByRole('img')).toBeInTheDocument()
  })

  it('draws nothing without points', () => {
    const { container } = render(<FrontierChart points={[]} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('copes with every point at one cost and loss', () => {
    render(<FrontierChart points={[{ threshold: 0.5, costUsd: 0.003, loss: 0, current: true }]} />)
    expect(screen.getByText('in use 0.50')).toBeInTheDocument()
  })
})
