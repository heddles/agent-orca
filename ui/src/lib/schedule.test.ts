import { describe, expect, it } from 'vitest'
import { cronOf, describeCron, whenOf, DEFAULT_WHEN, timeZones, formatIn } from './schedule'

describe('presets and cron', () => {
  it('writes the cron of each preset', () => {
    expect(cronOf({ ...DEFAULT_WHEN })).toBe('0 8 * * 1-5')
    expect(cronOf({ ...DEFAULT_WHEN, preset: 'weekly', weekday: 1, hour: 9, minute: 30 })).toBe('30 9 * * 1')
    expect(cronOf({ ...DEFAULT_WHEN, preset: 'daily', hour: 22, minute: 5 })).toBe('5 22 * * *')
    expect(cronOf({ ...DEFAULT_WHEN, preset: 'hourly', minute: 15 })).toBe('15 * * * *')
    expect(cronOf({ ...DEFAULT_WHEN, preset: 'custom', cron: ' */10 * * * * ' })).toBe('*/10 * * * *')
  })

  it('reads a cron back into the preset that writes it', () => {
    for (const cron of ['0 8 * * 1-5', '30 9 * * 1', '5 22 * * *', '15 * * * *']) {
      expect(cronOf(whenOf(cron))).toBe(cron)
      expect(whenOf(cron).preset).not.toBe('custom')
    }
  })

  it('keeps anything else as custom', () => {
    for (const cron of ['*/10 * * * *', '0 8 1 * *', '0 8 * * 1,3', '0 8 * * *  *', '0 24 * * *', 'nope']) {
      expect(whenOf(cron)).toMatchObject({ preset: 'custom', cron })
    }
  })

  it('describes a schedule in words', () => {
    expect(describeCron('0 8 * * 1-5')).toBe('Every weekday at 08:00')
    expect(describeCron('30 9 * * 1')).toBe('Every Monday at 09:30')
    expect(describeCron('5 22 * * *')).toBe('Every day at 22:05')
    expect(describeCron('15 * * * *')).toBe('Every hour at :15')
    expect(describeCron('*/10 * * * *')).toBe('*/10 * * * *')
  })
})

describe('time zones and times', () => {
  it('lists UTC and keeps the current zone available', () => {
    const z = timeZones('Mars/Olympus')
    expect(z[0]).toBe('Mars/Olympus')
    expect(z).toContain('UTC')
  })

  it('shows a time in the given zone', () => {
    expect(formatIn('2026-11-16T07:00:00Z', 'Europe/Berlin')).toContain('08:00')
    expect(formatIn(undefined, 'UTC')).toBe('')
    expect(formatIn('garbage', 'UTC')).toBe('garbage')
  })
})
