import { describe, it, expect } from 'vitest'
import {
  FAILED_SHARE_THRESHOLD,
  chartSummary,
  failedShare,
  failedShareHigh,
  outcomesInChart,
  sortedEntries,
  splitRunRef,
  usdNum,
} from './costMath'
import type { CostDay } from '../api/sse'

describe('usdNum', () => {
  it('parses numeric strings and coerces junk to 0', () => {
    expect(usdNum('1.2500')).toBe(1.25)
    expect(usdNum('')).toBe(0)
    expect(usdNum(undefined)).toBe(0)
    expect(usdNum('NaN')).toBe(0)
    expect(usdNum('abc')).toBe(0)
  })
})

describe('failedShare', () => {
  it('is the failed fraction of total spend', () => {
    expect(failedShare({ totalUSD: '10.0000', failedRunUSD: '2.5000' })).toBeCloseTo(0.25)
  })

  it('is 0 when nothing was spent (no division by zero)', () => {
    expect(failedShare({ totalUSD: '0.0000', failedRunUSD: '0.0000' })).toBe(0)
  })

  it('never exceeds 1', () => {
    expect(failedShare({ totalUSD: '1.0000', failedRunUSD: '5.0000' })).toBe(1)
  })

  it('flags only above the threshold', () => {
    const at = { totalUSD: '100', failedRunUSD: String(FAILED_SHARE_THRESHOLD * 100) }
    expect(failedShareHigh(at)).toBe(false)
    expect(failedShareHigh({ totalUSD: '100', failedRunUSD: '21' })).toBe(true)
  })
})

describe('sortedEntries', () => {
  it('orders largest first with a stable tie-break by name', () => {
    expect(sortedEntries({ b: '1.0', a: '1.0', c: '5.0' })).toEqual([
      ['c', '5.0'],
      ['a', '1.0'],
      ['b', '1.0'],
    ])
  })
})

describe('outcomesInChart / chartSummary', () => {
  const days: CostDay[] = [
    { date: '2026-10-20', usd: '3.0000', byOutcome: { Succeeded: '3.0000' } },
    { date: '2026-10-21', usd: '6.0000', byOutcome: { Succeeded: '5.0000', Failed: '1.0000', Pending: '0.0000' } },
  ]

  it('lists outcomes by total spend', () => {
    expect(outcomesInChart(days)).toEqual(['Succeeded', 'Failed', 'Pending'])
  })

  it('describes each day and its non-zero outcomes for screen readers', () => {
    const s = chartSummary(days)
    expect(s).toContain('2026-10-20 $3.0000 (Succeeded $3.0000)')
    expect(s).toContain('2026-10-21 $6.0000 (Succeeded $5.0000, Failed $1.0000)')
    expect(s).not.toContain('Pending')
  })

  it('has a message for an empty chart', () => {
    expect(chartSummary([])).toContain('no data')
  })
})

describe('splitRunRef', () => {
  it('splits namespace/name', () => {
    expect(splitRunRef('ns/run-1')).toEqual({ namespace: 'ns', name: 'run-1' })
  })
  it('handles a bare name', () => {
    expect(splitRunRef('run-1')).toEqual({ namespace: '', name: 'run-1' })
  })
})
