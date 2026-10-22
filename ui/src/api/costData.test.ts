import { describe, it, expect } from 'vitest'
import { emptyCostData, normalizeCostData } from './sse'

describe('normalizeCostData', () => {
  it('passes a full payload through', () => {
    const got = normalizeCostData({
      totalUSD: '25.2370',
      byAgent: { a: '1.0000' },
      byModel: { m: '2.0000' },
      byDay: [{ date: '2026-10-21', usd: '3.0000', byOutcome: { Failed: '1.0000' } }],
      byOutcome: { Failed: '1.0000' },
      untrackedCount: 2,
      untrackedSample: ['ns/a', 'ns/b'],
      inFlightUSD: '0.5000',
      avgRunUSD: '0.1426',
      failedRunUSD: '1.5050',
      archiveUnavailable: true,
    })
    expect(got.totalUSD).toBe('25.2370')
    expect(got.byDay).toEqual([{ date: '2026-10-21', usd: '3.0000', byOutcome: { Failed: '1.0000' } }])
    expect(got.untrackedCount).toBe(2)
    expect(got.untrackedSample).toEqual(['ns/a', 'ns/b'])
    expect(got.archiveUnavailable).toBe(true)
  })

  it('coerces an operator that predates the new fields', () => {
    const got = normalizeCostData({ totalUSD: '1.0000', byAgent: { a: '1.0000' }, byModel: {}, byDay: [] })
    expect(got.untrackedCount).toBe(0)
    expect(got.untrackedSample).toEqual([])
    expect(got.byOutcome).toEqual({})
    expect(got.inFlightUSD).toBe('0.0000')
    expect(got.avgRunUSD).toBe('0.0000')
    expect(got.failedRunUSD).toBe('0.0000')
    expect(got.archiveUnavailable).toBe(false)
  })

  it('coerces nulls (Go serialises nil maps and slices as null)', () => {
    const got = normalizeCostData({
      totalUSD: null, byAgent: null, byModel: null, byDay: null, byOutcome: null,
      untrackedCount: null, untrackedSample: null,
    })
    expect(got).toEqual(emptyCostData())
  })

  it('survives non-object payloads', () => {
    expect(normalizeCostData(null)).toEqual(emptyCostData())
    expect(normalizeCostData('oops')).toEqual(emptyCostData())
    expect(normalizeCostData([])).toEqual(emptyCostData())
  })

  it('drops malformed days and non-numeric amounts instead of throwing', () => {
    const got = normalizeCostData({
      totalUSD: 'NaN',
      byAgent: { good: '1.0000', bad: 'x', num: 2 },
      byDay: [null, 7, { usd: '1' }, { date: '2026-10-21', usd: 'zzz', byOutcome: { Failed: 'q' } }],
      untrackedCount: -3,
      untrackedSample: ['ns/a', 5, null],
    })
    expect(got.totalUSD).toBe('0.0000')
    expect(got.byAgent).toEqual({ good: '1.0000', bad: '0.0000', num: '2.0000' })
    expect(got.byDay).toEqual([{ date: '2026-10-21', usd: '0.0000', byOutcome: { Failed: '0.0000' } }])
    expect(got.untrackedCount).toBe(0)
    expect(got.untrackedSample).toEqual(['ns/a'])
  })

  it('accepts numeric amounts', () => {
    expect(normalizeCostData({ totalUSD: 1.5 }).totalUSD).toBe('1.5000')
  })
})
