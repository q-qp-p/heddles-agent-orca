import { describe, expect, it } from 'vitest'
import { toYAML } from './yaml'

describe('toYAML', () => {
  it('renders the merge patches the operator proposes', () => {
    expect(toYAML({ spec: { capabilityRouting: { 'stage:chat': 'weak' } } })).toBe(
      'spec:\n  capabilityRouting:\n    "stage:chat": weak\n',
    )
  })

  it('renders null, which a merge patch uses to delete a key', () => {
    expect(toYAML({ spec: { capabilityRouting: { 'stage:chat': null } } })).toBe(
      'spec:\n  capabilityRouting:\n    "stage:chat": null\n',
    )
  })

  it('renders arrays of objects whole, as a merge patch replaces them', () => {
    const out = toYAML({ spec: { providers: [{ name: 'strong', weight: 50 }, { name: 'weak', weight: 50, routingHint: 'cheap' }], fallbackChain: ['weak', 'other'] } })
    expect(out).toBe(
      [
        'spec:',
        '  providers:',
        '    - name: strong',
        '      weight: 50',
        '    - name: weak',
        '      weight: 50',
        '      routingHint: cheap',
        '  fallbackChain:',
        '    - weak',
        '    - other',
        '',
      ].join('\n'),
    )
  })

  it('renders numbers and booleans as themselves and quotes strings that would read as something else', () => {
    expect(toYAML({ a: 0.5, b: true, c: 'true', d: '12', e: 'has space', f: '', g: 'it\'s' })).toBe(
      'a: 0.5\nb: true\nc: "true"\nd: "12"\ne: "has space"\nf: ""\ng: "it\'s"\n',
    )
  })

  it('renders empty containers inline', () => {
    expect(toYAML({ a: [], b: {} })).toBe('a: []\nb: {}\n')
  })

  it('renders what JSON.parse gives back, so a patch round-trips through YAML-shaped text', () => {
    const patch = JSON.parse('{"spec":{"switchyard":{"classifier":{"base_threshold":0.3}}}}')
    expect(toYAML(patch)).toBe('spec:\n  switchyard:\n    classifier:\n      base_threshold: 0.3\n')
  })
})
